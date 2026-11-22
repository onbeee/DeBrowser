package service

// ProxyService is the redesigned proxy module: one Mihomo core,
// imported configuration sources (subscriptions and local
// files), the nodes parsed out of them, and the one
// authoritative instance -> node binding shared by the proxy
// page and the instance pages.
//
// The design is deliberately small:
//   - Import parses the source's `proxies:` list in Go and
//     stores each node's definition in the database; the node
//     list is therefore plain data, available whether or not
//     the core runs.
//   - The generated core configuration is just every node plus
//     one loopback listener per binding, each listener naming
//     its instance's node via the core's native per-listener
//     `proxy:` selection. No rules, no groups, no providers.
//   - The database is authoritative. Changes made while the
//     core runs are pushed by regenerating the configuration
//     and asking the core to reload; a reload failure is
//     reported (the change stays, and the launch gate's live
//     probe keeps any unserved binding from being used).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	"gopkg.in/yaml.v3"
)

const (
	proxyControllerPort  = 19097
	proxyListenerBase    = 21001
	proxyListenerSpan    = 1000
	proxyMaxSourceBytes  = 32 << 20
	proxyMaxFetchedBytes = 16 << 20
	proxyMaxNodes        = 2000
)

// ProxyService runs the proxy module. Its exported methods are
// the Wails-bound API used by the proxy page and the instance
// pages alike.
type ProxyService struct {
	store  *store.Store
	layout paths.Layout
	log    *slog.Logger
	vault  *secrets.Store

	// Auditor, when set, receives one operation-audit record
	// per user-initiated mutation (Stage 5B pattern).
	Auditor *logging.System

	// SourceFilePicker supplies the local file for AddFileSource
	// (wired to the native dialog in internal/app; a field so
	// the service layer stays GUI-free and testable).
	SourceFilePicker func() (string, error)

	mu      sync.Mutex
	manager *mihomo.Manager
	secret  string

	// fetchSubscription downloads a subscription body; a field
	// so tests can serve fixtures without a network.
	fetchSubscription func(ctx context.Context, rawURL string) ([]byte, error)
}

func NewProxyService(db *database.DB, layout paths.Layout, vault *secrets.Store, log *slog.Logger) *ProxyService {
	return &ProxyService{
		store:             store.New(db),
		layout:            layout,
		log:               log,
		vault:             vault,
		fetchSubscription: fetchSubscriptionHTTP,
	}
}

// ServiceShutdown stops the managed core when DeBrowser exits.
// Browsers the user started are not touched — only the core
// process DeBrowser itself started.
func (s *ProxyService) ServiceShutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manager == nil {
		return nil
	}
	return s.manager.Stop()
}

// --- views ---

// CoreStatusView is the core's state for the page header.
type CoreStatusView struct {
	Running bool   `json:"running"`
	Version string `json:"version"`
	Message string `json:"message"`
}

// SourceView is one imported source plus its node count.
type SourceView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	NodeCount int    `json:"nodeCount"`
	CreatedAt string `json:"createdAt"`
}

// NodeView is one node for the node list; the source name rides
// along so same-named nodes from different sources stay
// distinguishable.
type NodeView struct {
	ID         string `json:"id"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	Name       string `json:"name"`
	Type       string `json:"type"`
}

// BindingView is one instance's binding for display: the node
// it uses and the listener port dedicated to it.
type BindingView struct {
	InstanceID string `json:"instanceId"`
	NodeID     string `json:"nodeId"`
	NodeName   string `json:"nodeName"`
	SourceName string `json:"sourceName"`
	ListenPort int    `json:"listenPort"`
}

// --- core lifecycle ---

func (s *ProxyService) initCoreLocked() error {
	if err := os.MkdirAll(s.layout.Mihomo, 0o750); err != nil {
		return fmt.Errorf("创建代理核心目录失败：%w", err)
	}
	if s.secret == "" {
		secretPath := filepath.Join(s.layout.Mihomo, "controller-secret.txt")
		if data, err := os.ReadFile(secretPath); err == nil && len(strings.TrimSpace(string(data))) >= 16 {
			s.secret = strings.TrimSpace(string(data))
		} else {
			buf := make([]byte, 24)
			if _, err := rand.Read(buf); err != nil {
				return err
			}
			s.secret = hex.EncodeToString(buf)
			if err := os.WriteFile(secretPath, []byte(s.secret), 0o600); err != nil {
				return fmt.Errorf("保存核心控制密钥失败：%w", err)
			}
		}
	}
	if s.manager == nil {
		s.manager = mihomo.NewManager(s.layout.Mihomo)
	}
	return nil
}

func (s *ProxyService) client() *mihomo.Client {
	return mihomo.NewClient(proxyControllerPort, s.secret)
}

// coreRunningLocked reports whether DeBrowser's own core is up
// and answering with the shared secret.
func (s *ProxyService) coreRunningLocked() bool {
	if s.manager == nil || !s.manager.Running() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.client().Version(ctx) == nil
}

// GetCoreStatus reports the core's state. A core process that
// is running but not answering is reported as an error state,
// never as "running".
func (s *ProxyService) GetCoreStatus() CoreStatusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.initCoreLocked(); err != nil {
		return CoreStatusView{Message: err.Error()}
	}
	if s.coreRunningLocked() {
		return CoreStatusView{Running: true, Version: mihomo.CoreVersion}
	}
	if s.manager.Running() {
		msg := "代理核心进程在运行但没有响应"
		if werr := s.manager.WaitErr(); werr != nil {
			msg = fmt.Sprintf("代理核心已退出：%v", werr)
		}
		return CoreStatusView{Version: mihomo.CoreVersion, Message: msg}
	}
	// Not ours, but something answers with our secret? Cannot
	// happen (the secret is private to this installation). If
	// the port is occupied by a foreign program, starting will
	// say so; the stopped state stays plain.
	return CoreStatusView{Version: mihomo.CoreVersion}
}

// StartCore generates the configuration from the current
// database state and starts the core.
func (s *ProxyService) StartCore(operationID string) (CoreStatusView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.core_start", "proxy_core", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	var view CoreStatusView
	err := func() error {
		if err := s.initCoreLocked(); err != nil {
			return err
		}
		if s.coreRunningLocked() {
			view = CoreStatusView{Running: true, Version: mihomo.CoreVersion}
			return nil
		}
		if err := s.regenerateLocked(context.Background()); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := s.manager.Start(ctx, proxyControllerPort, s.client()); err != nil {
			return err
		}
		view = CoreStatusView{Running: true, Version: mihomo.CoreVersion}
		return nil
	}()
	finishOp(op, err)
	if err != nil {
		return CoreStatusView{Version: mihomo.CoreVersion, Message: err.Error()}, err
	}
	return view, nil
}

// StopCore stops the managed core.
func (s *ProxyService) StopCore(operationID string) (CoreStatusView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.core_stop", "proxy_core", "")
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.manager != nil {
		err = s.manager.Stop()
	}
	finishOp(op, err)
	if err != nil {
		return CoreStatusView{Version: mihomo.CoreVersion, Message: err.Error()}, err
	}
	return CoreStatusView{Version: mihomo.CoreVersion}, nil
}

// --- configuration generation ---

func coreNodeName(nodeID string) string { return "n-" + nodeID }

func (s *ProxyService) configPath() string {
	return filepath.Join(s.layout.Mihomo, "config.yaml")
}

func (s *ProxyService) sourcesDir() string {
	return filepath.Join(s.layout.Mihomo, "sources")
}

// regenerateLocked rewrites the core configuration from the
// database. Callers hold s.mu.
func (s *ProxyService) regenerateLocked(ctx context.Context) error {
	nodes, err := s.store.ListProxyNodes(ctx)
	if err != nil {
		return err
	}
	bindings, err := s.store.ListProxyBindings(ctx)
	if err != nil {
		return err
	}
	defs := make([]mihomo.NodeDef, 0, len(nodes))
	for _, n := range nodes {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(n.ConfigJSON), &cfg); err != nil {
			return fmt.Errorf("节点「%s」的配置数据损坏：%w", n.Name, err)
		}
		defs = append(defs, mihomo.NodeDef{CoreName: coreNodeName(n.ID), Config: cfg})
	}
	bdefs := make([]mihomo.BindingDef, 0, len(bindings))
	for _, b := range bindings {
		bdefs = append(bdefs, mihomo.BindingDef{Port: b.ListenPort, NodeCoreName: coreNodeName(b.NodeID)})
	}
	content := mihomo.Generate(defs, bdefs, proxyControllerPort, s.secret)
	if err := os.MkdirAll(s.layout.Mihomo, 0o750); err != nil {
		return err
	}
	tmp := s.configPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.configPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// reloadLocked regenerates the configuration and, when the core
// is running, asks it to reload. Callers hold s.mu.
func (s *ProxyService) reloadLocked(ctx context.Context) error {
	if err := s.regenerateLocked(ctx); err != nil {
		return err
	}
	if !s.coreRunningLocked() {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.client().ReloadConfig(rctx, s.configPath()); err != nil {
		return fmt.Errorf("核心重载配置失败（修改已保存，重启代理核心后生效）：%w", err)
	}
	return nil
}

// --- source import ---

type parsedNode struct {
	name string
	cfg  map[string]any
}

// parseProxyNodes extracts the node list from a Clash/Mihomo
// YAML document (a full configuration or a proxies-only file —
// both carry the nodes under the same top-level `proxies:`).
// URI-list / Base64 subscription formats are rejected with a
// specific message: the core cannot use them either.
func parseProxyNodes(data []byte) ([]parsedNode, error) {
	if looksLikeURIList(data) {
		return nil, fmt.Errorf("该内容是 URI/Base64 格式的订阅，DeBrowser 仅支持 Clash/Mihomo 的 YAML 配置格式")
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("配置文件不是有效的 YAML：%w", err)
	}
	if len(doc.Proxies) == 0 {
		return nil, fmt.Errorf("配置中没有找到代理节点（proxies 列表为空或不存在）")
	}
	if len(doc.Proxies) > proxyMaxNodes {
		return nil, fmt.Errorf("配置中的节点数量超过上限（%d）", proxyMaxNodes)
	}
	seen := map[string]bool{}
	out := make([]parsedNode, 0, len(doc.Proxies))
	for _, raw := range doc.Proxies {
		name, _ := raw["name"].(string)
		typ, _ := raw["type"].(string)
		if strings.TrimSpace(name) == "" || strings.TrimSpace(typ) == "" {
			return nil, fmt.Errorf("配置中存在缺少名称或类型的节点，无法导入")
		}
		if seen[name] {
			return nil, fmt.Errorf("配置中存在同名节点「%s」，同一配置内的节点名称必须唯一", name)
		}
		seen[name] = true
		out = append(out, parsedNode{name: name, cfg: raw})
	}
	return out, nil
}

func looksLikeURIList(data []byte) bool {
	text := strings.TrimSpace(string(data))
	if strings.Contains(text, "://") && !strings.Contains(text, "proxies:") {
		return true
	}
	// Base64 blobs decode to URI lists; a YAML document never
	// consists of one long unbroken token.
	if !strings.ContainsAny(text, "\n ") && len(text) > 64 {
		return true
	}
	return false
}

func fetchSubscriptionHTTP(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mihomo/"+strings.TrimPrefix(mihomo.CoreVersion, "v"))
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("服务器返回 %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, proxyMaxFetchedBytes))
}

// insertSourceLocked stores a parsed source and its nodes in
// one transaction. Callers hold s.mu and have already arranged
// the vault entry / file copy the row points at.
func (s *ProxyService) insertSourceLocked(ctx context.Context, src model.ProxySource, nodes []parsedNode) error {
	return s.store.WithTx(ctx, func(tx *store.Store) error {
		if err := tx.CreateProxySource(ctx, src); err != nil {
			return err
		}
		for _, n := range nodes {
			id, err := store.NewID()
			if err != nil {
				return err
			}
			cfgJSON, err := json.Marshal(n.cfg)
			if err != nil {
				return err
			}
			if err := tx.CreateProxyNode(ctx, model.ProxyNode{
				ID: id, SourceID: src.ID, Name: n.name, ConfigJSON: string(cfgJSON),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddSubscriptionSource downloads a subscription, parses its
// nodes, and only then records anything: a failed download or
// parse leaves no trace. The URL itself is stored exclusively
// in the credential vault.
func (s *ProxyService) AddSubscriptionSource(operationID, name, rawURL string) (SourceView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_add_subscription", "proxy_source", "")
	view, err := s.addSubscriptionSource(name, rawURL)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) addSubscriptionSource(name, rawURL string) (SourceView, error) {
	name = strings.TrimSpace(name)
	rawURL = strings.TrimSpace(rawURL)
	if name == "" {
		return SourceView{}, fmt.Errorf("请填写配置名称")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return SourceView{}, fmt.Errorf("订阅地址无效，请填写完整的 http/https 地址")
	}
	ctx := context.Background()
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	data, err := s.fetchSubscription(dctx, rawURL)
	cancel()
	if err != nil {
		return SourceView{}, fmt.Errorf("下载订阅失败：%w", err)
	}
	nodes, err := parseProxyNodes(data)
	if err != nil {
		return SourceView{}, fmt.Errorf("解析订阅失败：%w", err)
	}
	if s.vault == nil {
		return SourceView{}, fmt.Errorf("凭据 Vault 不可用，无法保存订阅地址")
	}
	ref, err := s.vault.Put([]byte(rawURL))
	if err != nil {
		return SourceView{}, fmt.Errorf("保存订阅地址到凭据 Vault 失败：%w", err)
	}
	id, err := store.NewID()
	if err != nil {
		_, _ = s.vault.DeleteCompensating(ref)
		return SourceView{}, err
	}
	src := model.ProxySource{ID: id, Name: name, Kind: model.ProxySourceKindSubscription, SecretRef: ref}
	s.mu.Lock()
	err = s.insertSourceLocked(ctx, src, nodes)
	if err != nil {
		s.mu.Unlock()
		_, _ = s.vault.DeleteCompensating(ref)
		return SourceView{}, err
	}
	s.vault.MarkCommitted(ref)
	err = s.reloadLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		// The source is recorded and valid; only the running
		// core did not pick it up yet (reloadLocked's message
		// says exactly that).
		return SourceView{}, err
	}
	return SourceView{ID: id, Name: name, Kind: src.Kind, NodeCount: len(nodes)}, nil
}

// AddFileSource imports a local Clash/Mihomo YAML file chosen
// through the native file dialog. DeBrowser keeps its own copy
// of the file; the original is never modified.
func (s *ProxyService) AddFileSource(operationID, name string) (SourceView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_add_file", "proxy_source", "")
	view, err := s.addFileSource(name)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) addFileSource(name string) (SourceView, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return SourceView{}, fmt.Errorf("请填写配置名称")
	}
	if s.SourceFilePicker == nil {
		return SourceView{}, fmt.Errorf("文件选择不可用")
	}
	path, err := s.SourceFilePicker()
	if err != nil {
		return SourceView{}, fmt.Errorf("选择文件失败：%w", err)
	}
	if path == "" {
		return SourceView{}, fmt.Errorf("未选择文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SourceView{}, fmt.Errorf("读取文件失败：%w", err)
	}
	if len(data) == 0 {
		return SourceView{}, fmt.Errorf("选择的文件为空")
	}
	if len(data) > proxyMaxSourceBytes {
		return SourceView{}, fmt.Errorf("选择的文件过大（上限 32 MB）")
	}
	nodes, err := parseProxyNodes(data)
	if err != nil {
		return SourceView{}, fmt.Errorf("解析配置文件失败：%w", err)
	}
	id, err := store.NewID()
	if err != nil {
		return SourceView{}, err
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.sourcesDir(), 0o750); err != nil {
		return SourceView{}, err
	}
	fileName := id + ".yaml"
	tmp := filepath.Join(s.sourcesDir(), fileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return SourceView{}, fmt.Errorf("保存配置文件副本失败：%w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.sourcesDir(), fileName)); err != nil {
		_ = os.Remove(tmp)
		return SourceView{}, fmt.Errorf("保存配置文件副本失败：%w", err)
	}
	src := model.ProxySource{ID: id, Name: name, Kind: model.ProxySourceKindFile, FileName: fileName}
	if err := s.insertSourceLocked(ctx, src, nodes); err != nil {
		_ = os.Remove(filepath.Join(s.sourcesDir(), fileName))
		return SourceView{}, err
	}
	if err := s.reloadLocked(ctx); err != nil {
		return SourceView{}, err
	}
	return SourceView{ID: id, Name: name, Kind: src.Kind, NodeCount: len(nodes)}, nil
}

// RefreshSource re-downloads a subscription and replaces its
// node set. A failed download or parse keeps the old nodes.
// Bindings to nodes that no longer exist are removed with
// them (the database cascades), which the pages then show as
// 未设置.
func (s *ProxyService) RefreshSource(operationID, id string) (SourceView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_refresh", "proxy_source", id)
	view, err := s.refreshSource(id)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) refreshSource(id string) (SourceView, error) {
	ctx := context.Background()
	src, err := s.store.GetProxySource(ctx, id)
	if err != nil {
		return SourceView{}, fmt.Errorf("配置不存在")
	}
	if src.Kind != model.ProxySourceKindSubscription {
		return SourceView{}, fmt.Errorf("本地配置文件不能在线更新，请删除后重新添加")
	}
	if s.vault == nil || src.SecretRef == "" {
		return SourceView{}, fmt.Errorf("订阅地址不可用（凭据 Vault 中缺失）")
	}
	plain, err := s.vault.Get(src.SecretRef)
	if err != nil {
		return SourceView{}, fmt.Errorf("读取订阅地址失败：%w", err)
	}
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	data, err := s.fetchSubscription(dctx, string(plain))
	cancel()
	if err != nil {
		return SourceView{}, fmt.Errorf("下载订阅失败（已保留原有节点）：%w", err)
	}
	nodes, err := parseProxyNodes(data)
	if err != nil {
		return SourceView{}, fmt.Errorf("解析订阅失败（已保留原有节点）：%w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.store.WithTx(ctx, func(tx *store.Store) error {
		if err := tx.DeleteProxyNodesOfSource(ctx, id); err != nil {
			return err
		}
		for _, n := range nodes {
			nid, err := store.NewID()
			if err != nil {
				return err
			}
			cfgJSON, err := json.Marshal(n.cfg)
			if err != nil {
				return err
			}
			if err := tx.CreateProxyNode(ctx, model.ProxyNode{
				ID: nid, SourceID: id, Name: n.name, ConfigJSON: string(cfgJSON),
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return SourceView{}, err
	}
	if err := s.reloadLocked(ctx); err != nil {
		return SourceView{}, err
	}
	return SourceView{ID: src.ID, Name: src.Name, Kind: src.Kind, NodeCount: len(nodes)}, nil
}

// DeleteSource removes a source, its nodes and any bindings to
// them (database cascades). The file copy and the vault entry
// are cleaned up afterwards; their failure is logged, not
// reported as a failed deletion — the deletion has committed.
func (s *ProxyService) DeleteSource(operationID, id string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.source_delete", "proxy_source", id)
	err := s.deleteSource(id)
	finishOp(op, err)
	return err
}

func (s *ProxyService) deleteSource(id string) error {
	ctx := context.Background()
	src, err := s.store.GetProxySource(ctx, id)
	if err != nil {
		return fmt.Errorf("配置不存在")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.DeleteProxySource(ctx, id); err != nil {
		return err
	}
	if src.Kind == model.ProxySourceKindFile && src.FileName != "" {
		if err := os.Remove(filepath.Join(s.sourcesDir(), src.FileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("proxy: source file copy cleanup failed after deletion", "source", id, "error", err)
		}
	}
	if src.SecretRef != "" && s.vault != nil {
		if _, err := s.vault.DeleteIfUnreferenced(src.SecretRef, func(r string) (bool, error) {
			return s.store.SecretRefInUse(ctx, r)
		}); err != nil {
			s.log.Warn("proxy: source credential cleanup failed after deletion", "source", id, "error", err)
		}
	}
	if err := s.reloadLocked(ctx); err != nil {
		s.log.Warn("proxy: core reload after source deletion failed", "source", id, "error", err)
	}
	return nil
}

// --- reads ---

// ListSources returns all imported sources with node counts.
func (s *ProxyService) ListSources() ([]SourceView, error) {
	ctx := context.Background()
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.store.ListProxyNodes(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, n := range nodes {
		counts[n.SourceID]++
	}
	out := make([]SourceView, 0, len(sources))
	for _, src := range sources {
		out = append(out, SourceView{
			ID: src.ID, Name: src.Name, Kind: src.Kind,
			NodeCount: counts[src.ID], CreatedAt: src.CreatedAt,
		})
	}
	return out, nil
}

// ListNodes returns every imported node across all sources.
func (s *ProxyService) ListNodes() ([]NodeView, error) {
	ctx := context.Background()
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, src := range sources {
		names[src.ID] = src.Name
	}
	nodes, err := s.store.ListProxyNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		var cfg map[string]any
		_ = json.Unmarshal([]byte(n.ConfigJSON), &cfg)
		typ, _ := cfg["type"].(string)
		out = append(out, NodeView{
			ID: n.ID, SourceID: n.SourceID, SourceName: names[n.SourceID],
			Name: n.Name, Type: typ,
		})
	}
	return out, nil
}

// ListBindings returns every instance binding with display
// names resolved. This is the single binding read both pages
// use.
func (s *ProxyService) ListBindings() ([]BindingView, error) {
	ctx := context.Background()
	bindings, err := s.store.ListProxyBindings(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := s.store.ListProxyNodes(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := s.store.ListProxySources(ctx)
	if err != nil {
		return nil, err
	}
	nodeByID := map[string]model.ProxyNode{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
	}
	srcName := map[string]string{}
	for _, src := range sources {
		srcName[src.ID] = src.Name
	}
	out := make([]BindingView, 0, len(bindings))
	for _, b := range bindings {
		v := BindingView{InstanceID: b.InstanceID, NodeID: b.NodeID, ListenPort: b.ListenPort}
		if n, ok := nodeByID[b.NodeID]; ok {
			v.NodeName = n.Name
			v.SourceName = srcName[n.SourceID]
		}
		out = append(out, v)
	}
	return out, nil
}

// --- bindings ---

// allocatePortLocked picks the lowest free listener port: not
// held by another binding and not answering on loopback.
func (s *ProxyService) allocatePortLocked(ctx context.Context) (int, error) {
	used, err := s.store.UsedProxyPorts(ctx)
	if err != nil {
		return 0, err
	}
	for p := proxyListenerBase; p < proxyListenerBase+proxyListenerSpan; p++ {
		if used[p] {
			continue
		}
		if mihomo.ProbePort(p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("没有可用的代理监听端口")
}

// SetBinding binds an instance to a node (or moves it to a new
// node). An existing binding keeps its listener port; a new
// binding gets a fresh allocation. This is the single binding
// write both pages use.
func (s *ProxyService) SetBinding(operationID, instanceID, nodeID string) (BindingView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_set", "instance", instanceID)
	view, err := s.setBinding(instanceID, nodeID)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) setBinding(instanceID, nodeID string) (BindingView, error) {
	ctx := context.Background()
	if _, err := s.store.GetInstance(ctx, instanceID); err != nil {
		return BindingView{}, fmt.Errorf("实例不存在")
	}
	node, err := s.store.GetProxyNode(ctx, nodeID)
	if err != nil {
		return BindingView{}, fmt.Errorf("代理节点不存在")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	port := 0
	if existing, err := s.store.GetProxyBinding(ctx, instanceID); err == nil {
		port = existing.ListenPort
	} else if errors.Is(err, store.ErrNotFound) {
		p, err := s.allocatePortLocked(ctx)
		if err != nil {
			return BindingView{}, err
		}
		port = p
	} else {
		return BindingView{}, fmt.Errorf("读取实例现有代理绑定失败，已中止设置：%w", err)
	}
	if err := s.store.UpsertProxyBinding(ctx, model.InstanceProxyBinding{
		InstanceID: instanceID, NodeID: nodeID, ListenPort: port,
	}); err != nil {
		return BindingView{}, err
	}
	if err := s.reloadLocked(ctx); err != nil {
		return BindingView{}, err
	}
	srcName := ""
	if src, err := s.store.GetProxySource(ctx, node.SourceID); err == nil {
		srcName = src.Name
	}
	return BindingView{
		InstanceID: instanceID, NodeID: nodeID, NodeName: node.Name,
		SourceName: srcName, ListenPort: port,
	}, nil
}

// ClearBinding removes an instance's binding (the instance
// connects directly afterwards). Clearing an unbound instance
// is a no-op.
func (s *ProxyService) ClearBinding(operationID, instanceID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_clear", "instance", instanceID)
	err := s.clearBinding(instanceID)
	finishOp(op, err)
	return err
}

func (s *ProxyService) clearBinding(instanceID string) error {
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.DeleteProxyBinding(ctx, instanceID); err != nil {
		return err
	}
	return s.reloadLocked(ctx)
}

// ProxyPortForInstance is the instance launch hook wired in
// main: it returns the loopback port the starting instance
// must use as its proxy, 0 for a direct connection, or an
// error refusing the launch when the instance is bound but
// the core cannot serve the binding right now. A bound
// instance never silently falls back to direct.
func (s *ProxyService) ProxyPortForInstance(instanceID string) (int, error) {
	ctx := context.Background()
	b, err := s.store.GetProxyBinding(ctx, instanceID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取实例代理绑定失败：%w", err)
	}
	instName := instanceID
	if inst, err := s.store.GetInstance(ctx, instanceID); err == nil {
		instName = inst.Name
	}
	s.mu.Lock()
	running := false
	if err := s.initCoreLocked(); err == nil {
		running = s.coreRunningLocked()
	}
	s.mu.Unlock()
	if !running {
		return 0, fmt.Errorf("实例「%s」已绑定代理节点，但代理核心未启动；请先在代理配置页面启动核心，或取消该实例的代理绑定", instName)
	}
	if !mihomo.ProbeListener(b.ListenPort) {
		return 0, fmt.Errorf("实例「%s」的代理监听端口 %d 未就绪，请重启代理核心后重试", instName, b.ListenPort)
	}
	return b.ListenPort, nil
}
