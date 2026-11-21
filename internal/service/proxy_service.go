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
//     connections, rule matching, DNS, delay tests).
//
// One core serves every browser instance. Each bound instance
// gets its own loopback listener, its own select group (fed by
// its bound source's provider only) and its own sub-rules, so
// instances choose nodes independently and never interfere.
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

	mu      sync.Mutex // serializes configuration regeneration and core lifecycle
	manager *mihomo.Manager
	client  *mihomo.Client
	secret  string

	// coreStarterOverride substitutes the core start step in
	// tests (a field, like the instance service's launcher).
	coreStarterOverride func(ctx context.Context) error
}

const (
	// proxyControllerPort is the core's REST controller port on
	// loopback. It is deliberately not a common Clash port.
	proxyControllerPort = 19097
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
	Mode          string `json:"mode"` // "rule" | "global"
	Port          int    `json:"port"`
	NodeState     string `json:"nodeState"`     // "ok" | "missing" | "unknown"
	ListenerState string `json:"listenerState"` // "ok" | "down" | "unknown"
}

// NewProxyService builds the service. The vault carries the
// subscription URLs; it must be the same store main opened.
func NewProxyService(db *database.DB, layout paths.Layout, vault *secrets.Store, log *slog.Logger) *ProxyService {
	client := mihomo.NewClient(proxyControllerPort, "")
	s := &ProxyService{
		store:  store.New(db),
		layout: layout,
		log:    log,
		vault:  vault,
		client: client,
	}
	s.manager = mihomo.NewManager(layout.Mihomo, s.configPath(), client)
	return s
}

func (s *ProxyService) configPath() string {
	return filepath.Join(s.layout.Mihomo, "config.yaml")
}

func (s *ProxyService) sourcesDir() string {
	return filepath.Join(s.layout.Mihomo, "sources")
}

// controllerSecret loads the core controller secret, generating
// and persisting a random one on first use. The secret lives in a
// file inside the core working directory; it is never logged and
// never leaves this machine.
func (s *ProxyService) controllerSecret() (string, error) {
	if s.secret != "" {
		return s.secret, nil
	}
	path := filepath.Join(s.layout.Mihomo, "controller-secret.txt")
	if b, err := os.ReadFile(path); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			s.secret = v
			s.client = mihomo.NewClient(proxyControllerPort, v)
			s.manager = mihomo.NewManager(s.layout.Mihomo, s.configPath(), s.client)
			return v, nil
		}
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("生成核心控制密钥失败：%w", err)
	}
	v := hex.EncodeToString(raw[:])
	if err := os.MkdirAll(s.layout.Mihomo, 0o750); err != nil {
		return "", fmt.Errorf("创建核心工作目录失败：%w", err)
	}
	if err := os.WriteFile(path, []byte(v), 0o600); err != nil {
		return "", fmt.Errorf("保存核心控制密钥失败：%w", err)
	}
	s.secret = v
	s.client = mihomo.NewClient(proxyControllerPort, v)
	s.manager = mihomo.NewManager(s.layout.Mihomo, s.configPath(), s.client)
	return v, nil
}

// ServiceStartup starts the core in the background when the user
// already has proxy sources, so instance proxies are ready by the
// time a browser launches. Startup never blocks the application
// and never fails it: problems surface on the proxy page.
func (s *ProxyService) ServiceStartup(ctx context.Context) error {
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		sources, err := s.store.ListProxySources(c)
		if err != nil || len(sources) == 0 {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.ensureCoreRunning(c); err != nil {
			s.log.Warn("proxy core autostart failed", "error", err)
		}
	}()
	return nil
}

// ServiceShutdown stops the core process this run started. The
// user's own Clash is a different process and is never touched.
func (s *ProxyService) ServiceShutdown() error {
	return s.manager.Stop()
}

// coreRunning reports whether the core answers its controller
// with our credentials right now.
func (s *ProxyService) coreRunning(ctx context.Context) bool {
	if _, err := s.controllerSecret(); err != nil {
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
	if _, err := s.controllerSecret(); err != nil {
		view.State = "error"
		view.Message = err.Error()
		return view
	}
	if v, err := s.client.Version(ctx); err == nil {
		view.State = "running"
		view.Version = v
		return view
	}
	view.State = "stopped"
	if s.manager.Running() {
		view.State = "error"
		view.Message = "核心进程存在但控制接口无响应"
	} else if werr := s.manager.WaitErr(); werr != nil {
		view.Message = "核心已退出：" + werr.Error()
	}
	return view
}

// ensureCoreRunning is the single entry point every operation
// uses to get a running core; tests substitute it via
// coreStarterOverride so no real core (or download) is involved.
func (s *ProxyService) ensureCoreRunning(ctx context.Context) error {
	if s.coreStarterOverride != nil {
		return s.coreStarterOverride(ctx)
	}
	return s.ensureCore(ctx)
}

// ensureCore starts the core when it is not running, regenerating
// the configuration first, then applies the recorded selections.
func (s *ProxyService) ensureCore(ctx context.Context) error {
	if s.coreRunning(ctx) {
		return nil
	}
	if err := s.regenerate(ctx); err != nil {
		return err
	}
	// Validate with the core's own check before starting, so a
	// bad configuration is a clear error here instead of a core
	// that exits silently. Validation downloads the binary when
	// missing, which the start would need anyway.
	if err := mihomo.Validate(ctx, s.layout.Mihomo, s.configPath()); err != nil {
		return err
	}
	if err := s.manager.Start(ctx); err != nil {
		return err
	}
	return s.applySelections(ctx)
}

// StartCore starts the core (user action on the proxy page).
func (s *ProxyService) StartCore(operationID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.core_start", "mihomo_core", "")
	err := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return s.ensureCoreRunning(ctx)
	}()
	finishOp(op, err)
	return err
}

// StopCore stops the core process DeBrowser started.
func (s *ProxyService) StopCore(operationID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.core_stop", "mihomo_core", "")
	s.mu.Lock()
	err := s.manager.Stop()
	s.mu.Unlock()
	finishOp(op, err)
	return err
}

// regenerate rebuilds the runtime configuration from the database
// (sources + bindings) and writes it atomically. When the core is
// running it hot-reloads; a rejected reload restores the previous
// file and leaves the running configuration untouched.
func (s *ProxyService) regenerate(ctx context.Context) error {
	secret, err := s.controllerSecret()
	if err != nil {
		return err
	}
	input, err := s.buildConfigInput(ctx, secret)
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

// buildConfigInput assembles the generator input from the store.
// Subscription URLs are read from the vault here and only here.
func (s *ProxyService) buildConfigInput(ctx context.Context, secret string) (mihomo.ConfigInput, error) {
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return mihomo.ConfigInput{}, err
	}
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return mihomo.ConfigInput{}, err
	}
	input := mihomo.ConfigInput{ControllerPort: proxyControllerPort, Secret: secret}
	providerBySource := map[string]string{}
	for _, src := range sources {
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
	for _, b := range bindings {
		provider, ok := providerBySource[b.SourceID]
		if !ok {
			continue // source deleted without its bindings cannot happen (FK), defensive only
		}
		input.Bindings = append(input.Bindings, mihomo.BindingInput{
			InstanceID:   b.InstanceID,
			ProviderName: provider,
			NodeName:     b.NodeName,
			Mode:         b.Mode,
			Port:         b.Port,
		})
	}
	return input, nil
}

// applySelections pushes DeBrowser's recorded instance->node
// selections into the running core's groups.
func (s *ProxyService) applySelections(ctx context.Context) error {
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if err := s.client.SelectNode(ctx, mihomo.GroupName(b.InstanceID), b.NodeName); err != nil {
			// A missing node (subscription updated) must not fail
			// the whole apply: the binding stays recorded and is
			// reported as unavailable in the UI.
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
// vault; the database holds only the vault reference) and makes
// its nodes available through the core.
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
	s.vault.MarkCommitted(ref)

	s.mu.Lock()
	coreErr := s.ensureCoreRunning(ctx)
	if coreErr == nil {
		// The provider was just added by the configuration reload;
		// ask the core to fetch it now so node counts are real.
		_ = s.client.RefreshProvider(ctx, src.ProviderName)
	}
	s.mu.Unlock()
	view := s.sourceView(ctx, src)
	if coreErr != nil && view.State == "core-stopped" {
		view.State = "error"
		view.Message = "订阅已保存，但核心启动失败：" + coreErr.Error()
	}
	return view, nil
}

// AddFileSource imports a local Mihomo YAML file through the
// native picker. DeBrowser copies the file into its own sources
// directory; the user's original file is never modified.
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
	id, err := store.NewID()
	if err != nil {
		return SourceView{}, err
	}
	fileName := id + ".yaml"
	if err := s.writeSourceCopy(id, fileName, data); err != nil {
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
	coreErr := s.ensureCoreRunning(ctx)
	s.mu.Unlock()
	view := s.sourceView(ctx, src)
	if coreErr != nil && view.State == "core-stopped" {
		view.State = "error"
		view.Message = "文件已导入，但核心启动失败：" + coreErr.Error()
	}
	return view, nil
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

func (s *ProxyService) writeSourceCopy(id string, fileName string, data []byte) error {
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

// RefreshSource re-fetches a subscription through the core. The
// core keeps the previous nodes when the fetch fails, and
// instance bindings are never changed by an update.
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
		if err := s.ensureCoreRunning(ctx); err != nil {
			return err
		}
		if err := s.client.RefreshProvider(ctx, src.ProviderName); err != nil {
			return fmt.Errorf("订阅更新失败，已保留原有节点：%w", err)
		}
		return nil
	}()
	finishOp(op, err)
	return err
}

// ReimportFileSource replaces a file source's stored copy with a
// newly picked file.
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
	if err := s.writeSourceCopy(id, src.FileName, data); err != nil {
		return SourceView{}, err
	}
	s.mu.Lock()
	if s.coreRunning(ctx) {
		err = s.regenerate(ctx)
		if err == nil {
			_ = s.client.RefreshProvider(ctx, src.ProviderName)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return SourceView{}, err
	}
	return s.sourceView(ctx, src), nil
}

// DeleteSource removes a source. Sources with instances bound to
// their nodes cannot be deleted until the bindings are cleared,
// so an update or deletion can never silently re-point an
// instance at another node.
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
		if err := s.store.DeleteProxySource(ctx, id); err != nil {
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
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.regenerate(ctx)
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
// group delay call per source) and returns per-node results.
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
	if err := s.ensureCoreRunning(ctx); err != nil {
		return nil, err
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
		Mode: b.Mode, Port: b.Port, NodeState: "unknown", ListenerState: "unknown",
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

// SetBinding binds an instance to a node of a source. The binding
// is recorded first and kept even when the node is currently
// unavailable; the returned view reports the node's state.
func (s *ProxyService) SetBinding(operationID string, instanceID string, sourceID string, nodeName string, mode string) (BindingView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_set", "browser_instance", instanceID)
	view, err := s.setBinding(instanceID, sourceID, nodeName, mode)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) setBinding(instanceID string, sourceID string, nodeName string, mode string) (BindingView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if mode != model.ProxyModeRule && mode != model.ProxyModeGlobal {
		return BindingView{}, fmt.Errorf("代理模式无效")
	}
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
	if _, err := s.store.GetProxySource(ctx, sourceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BindingView{}, fmt.Errorf("代理来源不存在")
		}
		return BindingView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	b, err := s.store.UpsertBinding(ctx, model.InstanceProxyBinding{
		InstanceID: instanceID, SourceID: sourceID, NodeName: nodeName, Mode: mode, Port: port,
	})
	if err != nil {
		return BindingView{}, fmt.Errorf("保存代理绑定失败：%w", err)
	}
	if err := s.regenerate(ctx); err != nil {
		return BindingView{}, err
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

// ClearBinding removes an instance's proxy binding.
func (s *ProxyService) ClearBinding(operationID string, instanceID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_clear", "browser_instance", instanceID)
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.store.DeleteBinding(ctx, instanceID); err != nil {
			return err
		}
		return s.regenerate(ctx)
	}()
	finishOp(op, err)
	return err
}

// ProxyPortForInstance is the launch hook the instance service
// uses: the loopback proxy port bound to an instance, or 0 when
// the instance has no proxy binding. It never fails a launch.
func (s *ProxyService) ProxyPortForInstance(instanceID string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := s.store.GetBinding(ctx, instanceID)
	if err != nil {
		return 0
	}
	return b.Port
}
