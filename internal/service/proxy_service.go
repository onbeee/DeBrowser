package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/mihomo"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// ProxyService is the Stage 6C Mihomo proxy management service.
//
// Three layers, kept separate on purpose:
//   - the frontend only displays and collects intent;
//   - this service owns DeBrowser's business data (sources and
//     instance bindings in SQLite, subscription URLs in the
//     credential vault) and coordinates the core;
//   - the Mihomo core does all proxy work (node parsing,
//     connections, DNS, delay tests).
//
// One core serves every browser instance. Each bound instance
// gets its own loopback listener that hands its traffic straight
// to its own select group (fed by its bound source's provider
// only), so instances choose nodes independently and never
// interfere. DeBrowser keeps no rule set of its own.
//
// Lifecycle: the core is started ONLY by the user's explicit
// action on the proxy page — never automatically at application
// startup, never as a side effect of another operation — and
// DeBrowser stops only the core process it started itself.
type ProxyService struct {
	store  *store.Store
	layout paths.Layout
	log    *slog.Logger
	vault  *secrets.Store

	// Auditor, when set, receives structured operation records
	// for user-initiated mutations.
	Auditor *logging.System

	// SourceFilePicker opens the native file picker for importing
	// a local Mihomo YAML file. Injected by the app package; nil
	// means local file import is unavailable.
	SourceFilePicker func() (string, error)

	mu sync.Mutex // serializes configuration regeneration and core lifecycle

	// The controller secret, API client and process manager are
	// initialized exactly once (initOnce), even when the
	// frontend's first calls arrive concurrently.
	initOnce sync.Once
	initErr  error
	manager  *mihomo.Manager
	client   *mihomo.Client
	secret   string
}

const (
	// proxyControllerPort is the core's REST controller port on
	// loopback. It is deliberately not a common Clash port.
	proxyControllerPort = 19097
	// proxyValidatePort is the controller port of the short-lived
	// throwaway core used to validate imported YAML files.
	proxyValidatePort = 19098
	// proxyListenerBasePort is the first loopback port handed out
	// to instance listeners.
	proxyListenerBasePort = 21001
	// proxyListenerMaxPort bounds the allocation scan.
	proxyListenerMaxPort = 21999
)

// CoreStatusView is the proxy page's core status summary.
type CoreStatusView struct {
	State       string `json:"state"` // "running" | "stopped" | "error"
	Version     string `json:"version"`
	BinaryReady bool   `json:"binaryReady"`
	Message     string `json:"message"`
}

// SourceView is one proxy source as the frontend shows it. It
// never carries the subscription URL or any credential.
type SourceView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"` // "url" | "file"
	NodeCount      int    `json:"nodeCount"`
	BoundInstances int    `json:"boundInstances"`
	State          string `json:"state"` // "ok" | "empty" | "error" | "core-stopped"
	Message        string `json:"message"`
	CreatedAt      string `json:"createdAt"`
}

// NodeView is one proxy node of one source.
type NodeView struct {
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Alive      bool   `json:"alive"`
	Delay      int    `json:"delay"` // ms; 0 = not measured
}

// NodeDelayView is one delay test result.
type NodeDelayView struct {
	SourceID string `json:"sourceId"`
	Name     string `json:"name"`
	Delay    int    `json:"delay"` // ms; -1 = test failed / unreachable
}

// BindingView is one instance's proxy binding as the frontend
// shows it.
type BindingView struct {
	InstanceID    string `json:"instanceId"`
	InstanceName  string `json:"instanceName"`
	SourceID      string `json:"sourceId"`
	SourceName    string `json:"sourceName"`
	NodeName      string `json:"nodeName"`
	Port          int    `json:"port"`
	NodeState     string `json:"nodeState"`     // "ok" | "missing" | "unknown"
	ListenerState string `json:"listenerState"` // "ok" | "down" | "unknown"
}

// NewProxyService builds the service. The vault carries the
// subscription URLs; it must be the same store main opened.
func NewProxyService(db *database.DB, layout paths.Layout, vault *secrets.Store, log *slog.Logger) *ProxyService {
	return &ProxyService{
		store:  store.New(db),
		layout: layout,
		log:    log,
		vault:  vault,
	}
}

func (s *ProxyService) configPath() string {
	return filepath.Join(s.layout.Mihomo, "config.yaml")
}

func (s *ProxyService) sourcesDir() string {
	return filepath.Join(s.layout.Mihomo, "sources")
}

// initCore performs the one-time initialization of the controller
// secret, the API client and the process manager. Concurrent
// first calls (the proxy page loads status, sources, nodes and
// bindings in parallel) all wait for the same initialization, so
// the secret file, the client and the manager always agree on one
// secret. Later calls return the stored result immediately.
func (s *ProxyService) initCore() error {
	s.initOnce.Do(func() {
		s.initErr = s.loadControllerSecret()
	})
	return s.initErr
}

// loadControllerSecret loads the core controller secret,
// generating and persisting a random one on first use. The secret
// lives in a file inside the core working directory; it is never
// logged and never leaves this machine.
func (s *ProxyService) loadControllerSecret() error {
	path := filepath.Join(s.layout.Mihomo, "controller-secret.txt")
	if b, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			s.setCoreHandles(v)
			return nil
		}
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("生成核心控制密钥失败：%w", err)
	}
	v := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(s.layout.Mihomo, 0o750); err != nil {
		return fmt.Errorf("创建核心工作目录失败：%w", err)
	}
	if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
		return fmt.Errorf("保存核心控制密钥失败：%w", err)
	}
	s.setCoreHandles(v)
	return nil
}

func (s *ProxyService) setCoreHandles(secret string) {
	s.secret = secret
	s.client = mihomo.NewClient(proxyControllerPort, secret)
	s.manager = mihomo.NewManager(s.layout.Mihomo, s.configPath(), s.client, proxyControllerPort)
}

// ServiceShutdown stops the core process this DeBrowser run
// started (if any). The user's own Clash is a different process
// and is never touched. A run that never used the proxy features
// has nothing to stop.
func (s *ProxyService) ServiceShutdown() error {
	if s.manager == nil {
		return nil
	}
	return s.manager.Stop()
}

// coreRunning reports whether the core DeBrowser started answers
// its controller with our credentials right now.
func (s *ProxyService) coreRunning(ctx context.Context) bool {
	if s.initCore() != nil {
		return false
	}
	if !s.manager.Running() {
		return false
	}
	_, err := s.client.Version(ctx)
	return err == nil
}

// GetCoreStatus returns the core's status for the proxy page.
func (s *ProxyService) GetCoreStatus() CoreStatusView {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, binErr := os.Stat(mihomo.BinaryPath(s.layout.Mihomo))
	view := CoreStatusView{BinaryReady: binErr == nil}
	if err := s.initCore(); err != nil {
		view.State = "error"
		view.Message = err.Error()
		return view
	}
	if s.manager.Running() {
		if v, err := s.client.Version(ctx); err == nil {
			view.State = "running"
			view.Version = v
			return view
		}
		view.State = "error"
		view.Message = "核心进程存在但控制接口无响应"
		return view
	}
	if _, err := s.client.Version(ctx); err == nil {
		// Something answers with our secret but is not a process
		// this run started: never claim or adopt it.
		view.State = "error"
		view.Message = "控制端口上已有其他核心在运行（不是 DeBrowser 启动的），DeBrowser 不会接管它"
		return view
	}
	if mihomo.ProbePort(proxyControllerPort) {
		view.State = "error"
		view.Message = fmt.Sprintf("控制端口 %d 被其他程序占用，DeBrowser 无法在此端口启动核心；占用端口的程序不受影响", proxyControllerPort)
		return view
	}
	view.State = "stopped"
	if werr := s.manager.WaitErr(); werr != nil {
		view.Message = "核心已退出：" + werr.Error()
	}
	return view
}

// StartCore starts the core. It is the ONLY way the core starts:
// an explicit user action on the proxy page. DeBrowser startup
// never starts the core, and no other operation starts it as a
// side effect.
func (s *ProxyService) StartCore(operationID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.core_start", "mihomo_core", "")
	err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.initCore(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := s.regenerate(ctx); err != nil {
			return err
		}
		// Validate with the core's own check before starting, so
		// a bad configuration is a clear error here instead of a
		// core that exits silently. Validation downloads the
		// binary when missing, which the start would need anyway.
		if err := mihomo.Validate(ctx, s.layout.Mihomo, s.configPath()); err != nil {
			return err
		}
		if err := s.manager.Start(ctx); err != nil {
			return err
		}
		return s.applySelections(ctx)
	}()
	finishOp(op, err)
	return err
}

// StopCore stops the core process DeBrowser started. A stop that
// fails is reported as an error; the UI never shows a stopped
// state while our core is still running.
func (s *ProxyService) StopCore(operationID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.core_stop", "mihomo_core", "")
	err := func() error {
		if err := s.initCore(); err != nil {
			return err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.manager.Stop()
	}()
	finishOp(op, err)
	return err
}

// ---------------------------------------------------------------------------
// Runtime configuration
// ---------------------------------------------------------------------------

// proxyConfigState is one consistent snapshot of the business
// data the runtime configuration is generated from. Mutations
// that must keep the database and the running core in agreement
// build the PROPOSED state first, push it into the configuration
// (and the running core), and only then commit the database
// change.
type proxyConfigState struct {
	sources  []model.ProxySource
	bindings []model.InstanceProxyBinding
}

func (s *ProxyService) loadConfigState(ctx context.Context) (proxyConfigState, error) {
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return proxyConfigState{}, err
	}
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return proxyConfigState{}, err
	}
	return proxyConfigState{sources: sources, bindings: bindings}, nil
}

// regenerate rebuilds the runtime configuration from the current
// database state. Callers hold s.mu.
func (s *ProxyService) regenerate(ctx context.Context) error {
	state, err := s.loadConfigState(ctx)
	if err != nil {
		return err
	}
	return s.regenerateState(ctx, state)
}

// regenerateState renders the configuration for the given state
// and writes it atomically. When the core is running it
// hot-reloads; a rejected reload restores the previous file and
// leaves the running configuration untouched. Callers hold s.mu.
func (s *ProxyService) regenerateState(ctx context.Context, state proxyConfigState) error {
	if err := s.initCore(); err != nil {
		return err
	}
	input, err := s.buildConfigInput(ctx, state)
	if err != nil {
		return err
	}
	text, err := mihomo.Generate(input)
	if err != nil {
		return err
	}
	cfgPath := s.configPath()
	prev, _ := os.ReadFile(cfgPath)
	tmp := cfgPath + ".tmp"
	if err := os.MkdirAll(s.layout.Mihomo, 0o750); err != nil {
		return fmt.Errorf("创建核心工作目录失败：%w", err)
	}
	if err := os.WriteFile(tmp, []byte(text), 0o640); err != nil {
		return fmt.Errorf("写入核心配置失败：%w", err)
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("写入核心配置失败：%w", err)
	}
	if s.coreRunning(ctx) {
		if err := s.client.ReloadConfig(ctx, cfgPath); err != nil {
			// Roll the file back so the next start loads the same
			// configuration the core is still running.
			if prev != nil {
				_ = os.WriteFile(cfgPath, prev, 0o640)
			}
			return fmt.Errorf("核心拒绝了新配置，已保留原有配置：%w", err)
		}
		return s.applySelections(ctx)
	}
	return nil
}

// buildConfigInput assembles the generator input from a state
// snapshot. Subscription URLs are read from the vault here and
// only here.
func (s *ProxyService) buildConfigInput(ctx context.Context, state proxyConfigState) (mihomo.ConfigInput, error) {
	input := mihomo.ConfigInput{ControllerPort: proxyControllerPort, Secret: s.secret}
	providerBySource := map[string]string{}
	for _, src := range state.sources {
		si := mihomo.SourceInput{ProviderName: src.ProviderName, Kind: src.Kind, FileName: src.FileName}
		if src.Kind == model.ProxySourceKindURL {
			if s.vault == nil || src.SecretRef == "" {
				return mihomo.ConfigInput{}, fmt.Errorf("订阅「%s」的地址凭据缺失，无法生成核心配置，请删除后重新添加", src.Name)
			}
			raw, err := s.vault.Get(src.SecretRef)
			if err != nil {
				return mihomo.ConfigInput{}, fmt.Errorf("订阅「%s」的地址凭据无法读取，无法生成核心配置，请删除后重新添加", src.Name)
			}
			si.URL = string(raw)
		}
		providerBySource[src.ID] = src.ProviderName
		input.Sources = append(input.Sources, si)
	}
	for _, b := range state.bindings {
		provider, ok := providerBySource[b.SourceID]
		if !ok {
			continue // source deleted without its bindings cannot happen (FK), defensive only
		}
		input.Bindings = append(input.Bindings, mihomo.BindingInput{
			InstanceID:   b.InstanceID,
			ProviderName: provider,
			NodeName:     b.NodeName,
			Port:         b.Port,
		})
	}
	return input, nil
}

// applySelections pushes DeBrowser's recorded instance->node
// selections into the running core's groups. A binding whose node
// currently exists is selected; a binding whose node is gone (or
// whose provider cannot be read) is set to REJECT explicitly, so
// the instance refuses traffic instead of silently falling back
// to another node. Selection problems never fail the caller: the
// binding stays recorded and the UI reports the node as missing.
func (s *ProxyService) applySelections(ctx context.Context) error {
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return err
	}
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return err
	}
	providerBySource := map[string]string{}
	for _, src := range sources {
		providerBySource[src.ID] = src.ProviderName
	}
	for _, b := range bindings {
		target := "REJECT"
		if provider, ok := providerBySource[b.SourceID]; ok {
			if nodes, err := s.client.ProviderProxies(ctx, provider); err == nil {
				for _, n := range nodes {
					if n.Name == b.NodeName {
						target = b.NodeName
						break
					}
				}
			}
		}
		if err := s.client.SelectNode(ctx, mihomo.GroupName(b.InstanceID), target); err != nil {
			s.log.Warn("apply proxy selection failed", "instance", b.InstanceID, "error", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------

// providerNameFor derives the core provider name for a source ID.
func providerNameFor(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) > 8 {
		s = s[:8]
	}
	return "src-" + s
}

// sourceView renders one source for the frontend, joining live
// node information from the core when it is running.
func (s *ProxyService) sourceView(ctx context.Context, src model.ProxySource) SourceView {
	view := SourceView{
		ID: src.ID, Name: src.Name, Kind: src.Kind, NodeCount: -1,
		CreatedAt: src.CreatedAt, State: "core-stopped",
	}
	if n, err := s.store.CountBindingsForSource(ctx, src.ID); err == nil {
		view.BoundInstances = n
	}
	if !s.coreRunning(ctx) {
		view.Message = "核心未运行，节点数未知"
		return view
	}
	nodes, err := s.client.ProviderProxies(ctx, src.ProviderName)
	if err != nil {
		view.State = "error"
		view.Message = "核心未能加载该来源，请检查来源内容后重试更新"
		return view
	}
	view.NodeCount = len(nodes)
	if len(nodes) == 0 {
		view.State = "empty"
		if src.Kind == model.ProxySourceKindURL {
			view.Message = "未能从该订阅提取到任何节点：可能格式不受支持，或订阅内部结构无法提取"
		} else {
			view.Message = "该文件中没有可用的代理节点"
		}
		return view
	}
	view.State = "ok"
	return view
}

// ListSources returns all sources with live node counts.
func (s *ProxyService) ListSources() ([]SourceView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SourceView, 0, len(sources))
	for _, src := range sources {
		out = append(out, s.sourceView(ctx, src))
	}
	return out, nil
}

func validateSourceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("来源名称不能为空")
	}
	if len([]rune(name)) > 50 {
		return "", fmt.Errorf("来源名称过长")
	}
	return name, nil
}

// AddURLSource archives a subscription URL (in the credential
// vault; the database holds only the vault reference). Adding a
// source never starts the core; when the core is already running
// the new provider is loaded into it, and a failure there rolls
// the archive back so database and core stay consistent.
func (s *ProxyService) AddURLSource(operationID string, name string, rawURL string) (SourceView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_add_url", "proxy_source", "")
	view, err := s.addURLSource(name, rawURL)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) addURLSource(name string, rawURL string) (SourceView, error) {
	name, err := validateSourceName(name)
	if err != nil {
		return SourceView{}, err
	}
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return SourceView{}, fmt.Errorf("订阅地址无效：需要以 http:// 或 https:// 开头的完整地址")
	}
	if s.vault == nil {
		return SourceView{}, fmt.Errorf("凭据存储不可用，无法保存订阅地址")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ref, err := s.vault.Put([]byte(rawURL))
	if err != nil {
		return SourceView{}, fmt.Errorf("保存订阅地址失败：%w", err)
	}
	id, err := store.NewID()
	if err != nil {
		_, _ = s.vault.DeleteCompensating(ref)
		return SourceView{}, err
	}
	src, err := s.store.CreateProxySource(ctx, model.ProxySource{
		ID: id, Name: name, Kind: model.ProxySourceKindURL,
		SecretRef: ref, ProviderName: providerNameFor(id),
	})
	if err != nil {
		_, _ = s.vault.DeleteCompensating(ref)
		return SourceView{}, fmt.Errorf("保存订阅失败：%w", err)
	}
	s.mu.Lock()
	if s.coreRunning(ctx) {
		if err := s.regenerate(ctx); err != nil {
			// The core rejected the configuration with the new
			// source: roll the archive back.
			_ = s.store.DeleteProxySource(ctx, id)
			_, _ = s.vault.DeleteCompensating(ref)
			s.mu.Unlock()
			return SourceView{}, fmt.Errorf("添加订阅失败，核心配置更新未生效，已回滚：%w", err)
		}
		// Ask the core to fetch the new provider now so node
		// counts are real; a fetch failure keeps the (empty)
		// provider and is reported by the source view.
		_ = s.client.RefreshProvider(ctx, src.ProviderName)
	}
	s.mu.Unlock()
	s.vault.MarkCommitted(ref)
	return s.sourceView(ctx, src), nil
}

// AddFileSource imports a local Mihomo YAML file through the
// native picker. DeBrowser copies the file into its own sources
// directory; the user's original file is never modified. The
// content is validated by the core itself (see validateFileData)
// before it is accepted.
func (s *ProxyService) AddFileSource(operationID string, name string) (SourceView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_add_file", "proxy_source", "")
	view, err := s.addFileSource(name)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) addFileSource(name string) (SourceView, error) {
	name, err := validateSourceName(name)
	if err != nil {
		return SourceView{}, err
	}
	if s.SourceFilePicker == nil {
		return SourceView{}, fmt.Errorf("本地文件导入不可用")
	}
	picked, err := s.SourceFilePicker()
	if err != nil {
		return SourceView{}, fmt.Errorf("选择文件失败：%w", err)
	}
	if strings.TrimSpace(picked) == "" {
		return SourceView{}, fmt.Errorf("未选择文件")
	}
	data, err := readSourceFile(picked)
	if err != nil {
		return SourceView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := s.checkFileData(ctx, data); err != nil {
		return SourceView{}, err
	}
	id, err := store.NewID()
	if err != nil {
		return SourceView{}, err
	}
	fileName := id + ".yaml"
	if err := s.writeSourceCopy(fileName, data); err != nil {
		return SourceView{}, err
	}
	src, err := s.store.CreateProxySource(ctx, model.ProxySource{
		ID: id, Name: name, Kind: model.ProxySourceKindFile,
		FileName: fileName, ProviderName: providerNameFor(id),
	})
	if err != nil {
		os.Remove(filepath.Join(s.sourcesDir(), fileName))
		return SourceView{}, fmt.Errorf("保存本地来源失败：%w", err)
	}
	s.mu.Lock()
	if s.coreRunning(ctx) {
		if err := s.regenerate(ctx); err != nil {
			_ = s.store.DeleteProxySource(ctx, id)
			os.Remove(filepath.Join(s.sourcesDir(), fileName))
			s.mu.Unlock()
			return SourceView{}, fmt.Errorf("导入文件失败，核心配置更新未生效，已回滚：%w", err)
		}
	}
	s.mu.Unlock()
	return s.sourceView(ctx, src), nil
}

// readSourceFile reads and sanity-checks an imported YAML file.
// The check is deliberately shallow (the core is the real parser):
// a provider payload must carry a proxies list, anything else
// cannot yield nodes and is rejected here with a clear message.
func readSourceFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("文件为空，无法导入")
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("文件过大，无法导入")
	}
	if !strings.Contains(string(data), "proxies:") {
		return nil, fmt.Errorf("未在文件中找到代理节点（proxies），该文件无法作为节点来源导入")
	}
	return data, nil
}

func (s *ProxyService) writeSourceCopy(fileName string, data []byte) error {
	if err := os.MkdirAll(s.sourcesDir(), 0o750); err != nil {
		return fmt.Errorf("创建来源目录失败：%w", err)
	}
	tmp := filepath.Join(s.sourcesDir(), fileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return fmt.Errorf("保存文件副本失败：%w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.sourcesDir(), fileName)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("保存文件副本失败：%w", err)
	}
	return nil
}

// checkFileData validates candidate YAML content with the core
// itself before the content is accepted anywhere. The core's own
// config test does not parse provider payloads, so validation
// runs a short-lived throwaway core (its own temp directory, its
// own controller port, no listeners) whose only job is loading
// the candidate as a file provider and reporting its nodes. The
// throwaway process is always killed before this returns, and
// the running production core is never touched.
//
// When the core binary is not present yet there is nothing to
// validate with and the check is skipped (nil); the content is
// then judged by the production core when it first loads.
func (s *ProxyService) checkFileData(ctx context.Context, data []byte) error {
	bin := mihomo.BinaryPath(s.layout.Mihomo)
	if _, err := os.Stat(bin); err != nil {
		return nil
	}
	count, err := s.countFileNodes(ctx, bin, data)
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("该文件无法被核心加载，或未提取到任何可用节点（格式不受支持或内部结构无法提取）")
	}
	return nil
}

// countFileNodes loads data as a file provider in a throwaway
// core process and returns how many nodes the core extracted.
func (s *ProxyService) countFileNodes(ctx context.Context, bin string, data []byte) (int, error) {
	if err := s.initCore(); err != nil {
		return 0, err
	}
	if mihomo.ProbePort(proxyValidatePort) {
		return 0, fmt.Errorf("文件验证端口被占用，暂时无法验证文件内容")
	}
	dir, err := os.MkdirTemp(s.layout.Mihomo, "validate-*")
	if err != nil {
		return 0, fmt.Errorf("创建文件验证目录失败：%w", err)
	}
	defer os.RemoveAll(dir)
	candidate := filepath.Join(dir, "candidate.yaml")
	if err := os.WriteFile(candidate, data, 0o640); err != nil {
		return 0, fmt.Errorf("写入验证文件失败：%w", err)
	}
	cfg := fmt.Sprintf("log-level: warning\nmode: rule\nexternal-controller: %q\nsecret: %q\nproxy-providers:\n  candidate:\n    type: file\n    path: %q\nrules:\n  - \"MATCH,DIRECT\"\n",
		fmt.Sprintf("127.0.0.1:%d", proxyValidatePort), s.secret, candidate)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o640); err != nil {
		return 0, fmt.Errorf("写入验证配置失败：%w", err)
	}
	cmd := exec.Command(bin, "-d", dir, "-f", cfgPath)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动验证核心失败：%w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	client := mihomo.NewClient(proxyValidatePort, s.secret)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := client.Version(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("验证核心未能启动，无法验证文件内容")
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	// The provider's initial load completes with startup; poll
	// briefly so a slow first parse is not misread as empty.
	deadline = time.Now().Add(5 * time.Second)
	for {
		nodes, err := client.ProviderProxies(ctx, "candidate")
		if err == nil && len(nodes) > 0 {
			return len(nodes), nil
		}
		if time.Now().After(deadline) {
			return 0, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// RefreshSource re-fetches a subscription through the core. The
// core keeps the previous nodes when the fetch fails, and
// instance bindings are never changed by an update; afterwards
// the recorded selections are re-applied (a node that disappeared
// leaves its instance on REJECT, a node that returned restores
// its binding). Refreshing requires the core to be running: it is
// never started implicitly.
func (s *ProxyService) RefreshSource(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_refresh", "proxy_source", id)
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		src, err := s.store.GetProxySource(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("来源不存在")
			}
			return err
		}
		if src.Kind != model.ProxySourceKindURL {
			return fmt.Errorf("本地文件来源请使用重新导入来更新内容")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.coreRunning(ctx) {
			return fmt.Errorf("代理核心未开启，无法更新订阅；请先在代理配置页面手动开启代理")
		}
		if err := s.client.RefreshProvider(ctx, src.ProviderName); err != nil {
			return fmt.Errorf("订阅更新失败，已保留原有节点：%w", err)
		}
		return s.applySelections(ctx)
	}()
	finishOp(op, err)
	return err
}

// ReimportFileSource replaces a file source's stored copy with a
// newly picked file. The new content is validated by the core
// itself BEFORE anything is replaced (see checkFileData): a file
// the core cannot load, or one that yields no nodes, is rejected
// and the previous file and its nodes stay exactly as they were.
func (s *ProxyService) ReimportFileSource(operationID string, id string) (SourceView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_reimport", "proxy_source", id)
	view, err := s.reimportFileSource(id)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) reimportFileSource(id string) (SourceView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	src, err := s.store.GetProxySource(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return SourceView{}, fmt.Errorf("来源不存在")
		}
		return SourceView{}, err
	}
	if src.Kind != model.ProxySourceKindFile {
		return SourceView{}, fmt.Errorf("订阅来源请使用更新来刷新")
	}
	if s.SourceFilePicker == nil {
		return SourceView{}, fmt.Errorf("本地文件导入不可用")
	}
	picked, err := s.SourceFilePicker()
	if err != nil {
		return SourceView{}, fmt.Errorf("选择文件失败：%w", err)
	}
	if strings.TrimSpace(picked) == "" {
		return SourceView{}, fmt.Errorf("未选择文件")
	}
	data, err := readSourceFile(picked)
	if err != nil {
		return SourceView{}, err
	}
	// Validate first; the stored copy is touched only after the
	// core itself has proven it can load the new content.
	if err := s.checkFileData(ctx, data); err != nil {
		return SourceView{}, fmt.Errorf("新文件未通过核心验证，已保留原文件和原有节点：%w", err)
	}
	if err := s.writeSourceCopy(src.FileName, data); err != nil {
		return SourceView{}, err
	}
	s.mu.Lock()
	if s.coreRunning(ctx) {
		if err := s.client.RefreshProvider(ctx, src.ProviderName); err != nil {
			s.mu.Unlock()
			return SourceView{}, fmt.Errorf("新文件已通过验证并已替换，但核心刷新失败，核心仍在使用原有节点：%w", err)
		}
		_ = s.applySelections(ctx)
	}
	s.mu.Unlock()
	return s.sourceView(ctx, src), nil
}

// DeleteSource removes a source. Sources with instances bound to
// their nodes cannot be deleted until the bindings are cleared,
// so an update or deletion can never silently re-point an
// instance at another node. The runtime configuration is updated
// to the proposed state (without the source) BEFORE the database
// row is removed; if the core rejects it, nothing is deleted.
func (s *ProxyService) DeleteSource(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_delete", "proxy_source", id)
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		src, err := s.store.GetProxySource(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("来源不存在")
			}
			return err
		}
		if n, err := s.store.CountBindingsForSource(ctx, id); err != nil {
			return err
		} else if n > 0 {
			return fmt.Errorf("有 %d 个实例正在使用该来源的节点，请先在实例编辑中解除这些实例的代理绑定", n)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		kept := state.sources[:0]
		for _, other := range state.sources {
			if other.ID != id {
				kept = append(kept, other)
			}
		}
		state.sources = kept
		if err := s.regenerateState(ctx, state); err != nil {
			return fmt.Errorf("核心配置更新失败，来源未删除：%w", err)
		}
		if err := s.store.DeleteProxySource(ctx, id); err != nil {
			_ = s.regenerate(ctx) // restore the live configuration (best effort)
			return fmt.Errorf("删除来源失败：%w", err)
		}
		if src.Kind == model.ProxySourceKindFile && src.FileName != "" {
			os.Remove(filepath.Join(s.sourcesDir(), src.FileName))
		}
		// The provider's cache file may hold subscription content;
		// remove it with the source.
		os.Remove(filepath.Join(s.layout.Mihomo, "providers", src.ProviderName+".yaml"))
		if src.SecretRef != "" && s.vault != nil {
			ref := src.SecretRef
			_, _ = s.vault.DeleteIfUnreferenced(ref, func(r string) (bool, error) {
				if used, err := s.store.SecretRefInUse(ctx, r); err != nil || used {
					return used, err
				}
				rest, err := s.store.ListProxySources(ctx)
				if err != nil {
					return false, err
				}
				for _, other := range rest {
					if other.SecretRef == r {
						return true, nil
					}
				}
				return false, nil
			})
		}
		return nil
	}()
	finishOp(op, err)
	return err
}

// ---------------------------------------------------------------------------
// Nodes and delay tests
// ---------------------------------------------------------------------------

// ListNodes returns every node of every source from the running
// core. When the core is stopped the list is empty; the page
// shows the core status separately.
func (s *ProxyService) ListNodes() ([]NodeView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out := []NodeView{}
	if !s.coreRunning(ctx) {
		return out, nil
	}
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		nodes, err := s.client.ProviderProxies(ctx, src.ProviderName)
		if err != nil {
			continue // the source's own view reports the problem
		}
		for _, n := range nodes {
			out = append(out, NodeView{
				SourceID: src.ID, SourceName: src.Name,
				Name: n.Name, Type: n.Type, Alive: n.Alive, Delay: n.Delay,
			})
		}
	}
	return out, nil
}

// TestDelays measures every node's delay through the core (one
// group delay call per source) and returns per-node results. It
// requires the core to be running and never starts it.
func (s *ProxyService) TestDelays(operationID string) ([]NodeDelayView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.delay_test", "mihomo_core", "")
	out, err := s.testDelays()
	finishOp(op, err)
	return out, err
}

func (s *ProxyService) testDelays() ([]NodeDelayView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.coreRunning(ctx) {
		return nil, fmt.Errorf("代理核心未开启，无法测速；请先在代理配置页面手动开启代理")
	}
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return nil, err
	}
	out := []NodeDelayView{}
	for _, src := range sources {
		nodes, err := s.client.ProviderProxies(ctx, src.ProviderName)
		if err != nil {
			continue
		}
		delays, err := s.client.GroupDelay(ctx, mihomo.DelayGroupName(src.ProviderName), 5000)
		if err != nil {
			delays = map[string]int{}
		}
		for _, n := range nodes {
			d, ok := delays[n.Name]
			if !ok {
				d = -1
			}
			out = append(out, NodeDelayView{SourceID: src.ID, Name: n.Name, Delay: d})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Instance bindings
// ---------------------------------------------------------------------------

// nodeState reports whether a binding's node currently exists in
// the core ("ok"), is gone ("missing"), or cannot be determined
// because the core is stopped ("unknown").
func (s *ProxyService) nodeState(ctx context.Context, b model.InstanceProxyBinding, providerName string) string {
	if !s.coreRunning(ctx) {
		return "unknown"
	}
	nodes, err := s.client.ProviderProxies(ctx, providerName)
	if err != nil {
		return "unknown"
	}
	for _, n := range nodes {
		if n.Name == b.NodeName {
			return "ok"
		}
	}
	return "missing"
}

func (s *ProxyService) bindingView(ctx context.Context, b model.InstanceProxyBinding) BindingView {
	view := BindingView{
		InstanceID: b.InstanceID, SourceID: b.SourceID, NodeName: b.NodeName,
		Port: b.Port, NodeState: "unknown", ListenerState: "unknown",
	}
	providerName := ""
	if src, err := s.store.GetProxySource(ctx, b.SourceID); err == nil {
		view.SourceName = src.Name
		providerName = src.ProviderName
	}
	if inst, err := s.store.GetInstance(ctx, b.InstanceID); err == nil {
		view.InstanceName = inst.Name
	}
	if providerName != "" {
		view.NodeState = s.nodeState(ctx, b, providerName)
	}
	if s.coreRunning(ctx) {
		if mihomo.ProbePort(b.Port) {
			view.ListenerState = "ok"
		} else {
			view.ListenerState = "down"
		}
	}
	return view
}

// ListBindings returns every instance binding with live state.
func (s *ProxyService) ListBindings() ([]BindingView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]BindingView, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, s.bindingView(ctx, b))
	}
	return out, nil
}

// GetBinding returns one instance's binding. bound=false (with a
// nil error) means the instance uses no proxy.
func (s *ProxyService) GetBinding(instanceID string) (view BindingView, bound bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := s.store.GetBinding(ctx, instanceID)
	if errors.Is(err, store.ErrNotFound) {
		return BindingView{}, false, nil
	}
	if err != nil {
		return BindingView{}, false, err
	}
	return s.bindingView(ctx, b), true, nil
}

// SetBinding binds an instance to a node of a source. When the
// core is running the node must currently exist in that source —
// a binding to a node that is not there is rejected outright.
// Database and runtime configuration stay consistent: the new
// configuration (with the proposed binding) is generated and
// loaded FIRST, and the database row is written only after the
// core has accepted it; if anything fails, the previous binding
// and the previous configuration both stay in force.
func (s *ProxyService) SetBinding(operationID string, instanceID string, sourceID string, nodeName string) (BindingView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_set", "browser_instance", instanceID)
	view, err := s.setBinding(instanceID, sourceID, nodeName)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) setBinding(instanceID string, sourceID string, nodeName string) (BindingView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		return BindingView{}, fmt.Errorf("请选择代理节点")
	}
	if _, err := s.store.GetInstance(ctx, instanceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BindingView{}, fmt.Errorf("实例不存在")
		}
		return BindingView{}, err
	}
	src, err := s.store.GetProxySource(ctx, sourceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BindingView{}, fmt.Errorf("代理来源不存在")
		}
		return BindingView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coreRunning(ctx) {
		nodes, err := s.client.ProviderProxies(ctx, src.ProviderName)
		if err != nil {
			return BindingView{}, fmt.Errorf("核心未能加载来源「%s」，暂时无法设置绑定", src.Name)
		}
		found := false
		for _, n := range nodes {
			if n.Name == nodeName {
				found = true
				break
			}
		}
		if !found {
			return BindingView{}, fmt.Errorf("节点「%s」不在来源「%s」的当前节点列表中，无法绑定", nodeName, src.Name)
		}
	}
	port := 0
	if existing, err := s.store.GetBinding(ctx, instanceID); err == nil {
		port = existing.Port // an instance keeps its listener port across changes
	} else {
		p, err := s.allocatePort(ctx)
		if err != nil {
			return BindingView{}, err
		}
		port = p
	}
	proposed := model.InstanceProxyBinding{
		InstanceID: instanceID, SourceID: sourceID, NodeName: nodeName, Port: port,
	}
	state, err := s.loadConfigState(ctx)
	if err != nil {
		return BindingView{}, err
	}
	replaced := false
	for i := range state.bindings {
		if state.bindings[i].InstanceID == instanceID {
			state.bindings[i] = proposed
			replaced = true
		}
	}
	if !replaced {
		state.bindings = append(state.bindings, proposed)
	}
	if err := s.regenerateState(ctx, state); err != nil {
		return BindingView{}, err
	}
	b, err := s.store.UpsertBinding(ctx, proposed)
	if err != nil {
		_ = s.regenerate(ctx) // restore the live configuration (best effort)
		return BindingView{}, fmt.Errorf("保存代理绑定失败：%w", err)
	}
	return s.bindingView(ctx, b), nil
}

// allocatePort picks the lowest free listener port: not used by
// another binding and not occupied on loopback right now.
func (s *ProxyService) allocatePort(ctx context.Context) (int, error) {
	used, err := s.store.UsedBindingPorts(ctx)
	if err != nil {
		return 0, err
	}
	taken := map[int]bool{}
	for _, p := range used {
		taken[p] = true
	}
	for p := proxyListenerBasePort; p <= proxyListenerMaxPort; p++ {
		if taken[p] || mihomo.ProbePort(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("没有可用的本地监听端口")
}

// ClearBinding removes an instance's proxy binding, with the same
// consistency discipline as SetBinding: the configuration without
// the binding is loaded first, the database row is deleted only
// after the core has accepted it.
func (s *ProxyService) ClearBinding(operationID string, instanceID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_clear", "browser_instance", instanceID)
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		kept := state.bindings[:0]
		for _, b := range state.bindings {
			if b.InstanceID != instanceID {
				kept = append(kept, b)
			}
		}
		state.bindings = kept
		if err := s.regenerateState(ctx, state); err != nil {
			return fmt.Errorf("核心配置更新失败，绑定未清除：%w", err)
		}
		if err := s.store.DeleteBinding(ctx, instanceID); err != nil {
			_ = s.regenerate(ctx) // restore the live configuration (best effort)
			return err
		}
		return nil
	}()
	finishOp(op, err)
	return err
}

// ProxyPortForInstance is the launch hook the instance service
// uses. (0, nil) means the instance has no proxy binding and
// launches exactly as before. A bound instance whose proxy is not
// actually usable — the core has not been started manually, or
// its listener does not answer — returns an error, so the launch
// is refused with a clear message instead of starting a browser
// against a dead proxy port. The core is never started from here.
func (s *ProxyService) ProxyPortForInstance(instanceID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := s.store.GetBinding(ctx, instanceID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取实例代理绑定失败，实例未启动：%w", err)
	}
	if !s.coreRunning(ctx) {
		return 0, fmt.Errorf("该实例已绑定代理节点，但代理核心尚未开启。请先在「代理配置」页面手动开启代理，再启动该实例")
	}
	if !mihomo.ProbePort(b.Port) {
		return 0, fmt.Errorf("该实例的代理监听端口 127.0.0.1:%d 无响应，实例未启动。请检查「代理配置」页面的核心状态后重试", b.Port)
	}
	return b.Port, nil
}
