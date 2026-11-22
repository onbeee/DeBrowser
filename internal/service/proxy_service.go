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
//     connections, DNS, delay tests).
//
// One core serves every browser instance. Each bound instance
// gets its own loopback listener whose traffic enters the
// instance's own sub-rules: the sub-rules end at the instance's
// select group (fed by its bound source's provider only) in
// rule and global mode, or at DIRECT in direct mode, so
// instances choose nodes independently and never interfere.
// The generated configuration also carries DeBrowser's routing
// layer: 14 built-in rule-providers with per-set policies plus
// the user's custom top rules, compiled into each rule-mode
// instance's sub-rules (see mihomo.Generate). Subscription
// files' own rules and groups are never imported — a source
// contributes nodes only.
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

	// refreshOverride and selectOverride, when set, replace the
	// corresponding core calls. They are test seams only (nil in
	// production): refresh and selection failures inside the
	// running core cannot be provoked from outside it, and the
	// rollback and error-propagation paths must still be
	// exercised deterministically.
	refreshOverride func(ctx context.Context, provider string) error
	selectOverride  func(ctx context.Context, group, node string) error
	// groupNowOverride, when set, replaces the group-selection
	// read. Test seam only (nil in production), same rationale:
	// a group parked on REJECT by core-side drift cannot be
	// provoked deterministically from outside the core.
	groupNowOverride func(ctx context.Context, group string) (string, error)
	// coreRunningOverride and reloadOverride, when set, replace
	// the running-core check and the config reload. Test seams
	// only (nil in production), same rationale: regenerateState's
	// hot-reload rollback paths need a "running core" whose
	// reload and selection outcomes are scriptable.
	coreRunningOverride func() bool
	reloadOverride      func(ctx context.Context) error
	// geoipDownloadOverride, when set, replaces the GeoIP
	// database download. Test seam only (nil in production):
	// the real download depends on the network, and the
	// start-blocking failure path must be exercised
	// deterministically.
	geoipDownloadOverride func(ctx context.Context, dir string) error

	// InstanceStatus, when wired by main, returns an instance's
	// effective lifecycle status ("stopped", "starting",
	// "running", "stopping", "unknown", ...). Port reassignment
	// is allowed only from a confirmed "stopped"; the bindings
	// view also uses it to disable the action while an instance
	// runs. Nil (tests) means "no information" — views then
	// report not-running and reassignment skips the check only
	// when the lifecycle lock hook is also unwired.
	InstanceStatus func(instanceID string) (string, bool)
	// Lifecycle, when wired by main, is the SAME per-instance
	// lifecycle lock registry the instance service holds across
	// start, stop and deletion (see instance_lifecycle.go).
	// Port reassignment holds it for its whole check-and-update,
	// so the status it checks cannot change underneath it and
	// no start can read a half-updated port. Nil (tests) means
	// no locking.
	Lifecycle *InstanceLifecycle
}

// configNeedsGeoIP reports whether the given bindings make the
// generated configuration reference the GeoIP database: every
// rule-mode binding's sub-rules contain a GEOIP line, and the
// core refuses to load such a configuration without the
// database in its working directory.
func configNeedsGeoIP(bindings []model.InstanceProxyBinding) bool {
	for _, b := range bindings {
		if b.RouteMode == "" || b.RouteMode == model.RouteModeRule {
			return true
		}
	}
	return false
}

// ensureGeoIP makes sure the GeoIP database is in place before
// the core is started with — or reloaded into — a configuration
// that needs it. A database already present is reused untouched.
// A missing one is downloaded and validated by the mihomo
// package (temporary file + atomic rename). A failed download
// is a hard error: starting the core anyway would make IT fail
// the whole configuration load with a far less clear message,
// so the operation stops here instead.
func (s *ProxyService) ensureGeoIP(ctx context.Context, bindings []model.InstanceProxyBinding) error {
	if !configNeedsGeoIP(bindings) {
		return nil
	}
	if mihomo.GeoIPPresent(s.layout.Mihomo) {
		return nil
	}
	download := func(ctx context.Context, dir string) error {
		return mihomo.DownloadGeoIP(ctx, dir, mihomo.GeoIPDownloadURL)
	}
	if s.geoipDownloadOverride != nil {
		download = s.geoipDownloadOverride
	}
	if err := download(ctx, s.layout.Mihomo); err != nil {
		return fmt.Errorf("GeoIP 数据库下载失败，核心尚未启动（规则分流模式需要该数据库）：%w；请检查网络后重试", err)
	}
	if !mihomo.GeoIPPresent(s.layout.Mihomo) {
		return fmt.Errorf("GeoIP 数据库下载失败，核心尚未启动（规则分流模式需要该数据库）：下载未产生有效文件；请检查网络后重试")
	}
	return nil
}

// refreshProvider asks the core to re-fetch one proxy-provider.
func (s *ProxyService) refreshProvider(ctx context.Context, provider string) error {
	if s.refreshOverride != nil {
		return s.refreshOverride(ctx, provider)
	}
	return s.client.RefreshProvider(ctx, provider)
}

// selectNode sets one instance group's current node in the core.
func (s *ProxyService) selectNode(ctx context.Context, group, node string) error {
	if s.selectOverride != nil {
		return s.selectOverride(ctx, group, node)
	}
	return s.client.SelectNode(ctx, group, node)
}

// groupNow reads one group's currently selected member from the
// core: the bound node's name, or REJECT while it is unavailable.
func (s *ProxyService) groupNow(ctx context.Context, group string) (string, error) {
	if s.groupNowOverride != nil {
		return s.groupNowOverride(ctx, group)
	}
	return s.client.GroupNow(ctx, group)
}

const (
	// proxyControllerPort is the core's REST controller port on
	// loopback. It is deliberately not a common Clash port.
	proxyControllerPort = 19097
	// proxyValidatePort is the controller port of the short-lived
	// throwaway core used to validate imported YAML files.
	proxyValidatePort = 19098
	// proxyListenerBasePort is the DEFAULT first loopback port
	// handed out to instance listeners; the editable setting
	// (ProxySettings.ListenerPortStart) starts the allocation
	// scan, which spans 1000 ports from there.
	proxyListenerBasePort = 21001
)

// CoreStatusView is the proxy page's core status summary. It
// also carries the read-only facts the page's settings dialog
// shows (the pinned core version and the core data directory);
// the user-adjustable settings live in ProxySettingsView
// (proxy_rules_service.go).
type CoreStatusView struct {
	State       string `json:"state"` // "running" | "stopped" | "error"
	Version     string `json:"version"`
	BinaryReady bool   `json:"binaryReady"`
	Message     string `json:"message"`
	CoreVersion string `json:"coreVersion"` // pinned official Mihomo version
	DataDir     string `json:"dataDir"`     // the core's portable working directory
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
	Delay      int    `json:"delay"`   // ms; 0 = not measured
	Checked    bool   `json:"checked"` // the core has health-checked this node at least once
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
	RouteMode     string `json:"routeMode"`     // "rule" | "global" | "direct"
	NodeState     string `json:"nodeState"`     // "ok" | "down" | "missing" | "unknown"
	ListenerState string `json:"listenerState"` // "ok" | "down" | "unknown"
	// InstanceRunning reports whether the instance's browser may
	// currently be running (running, or state unconfirmed) — the
	// port-reassignment action is disabled then, because the
	// browser process still points at the old --proxy-server
	// port. Filled via the InstanceStatus hook when wired.
	InstanceRunning bool `json:"instanceRunning"`
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
	if s.coreRunningOverride != nil {
		return s.coreRunningOverride()
	}
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
	view := CoreStatusView{
		BinaryReady: binErr == nil,
		CoreVersion: mihomo.CoreVersion,
		DataDir:     s.layout.Mihomo,
	}
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
		// Prepare the GeoIP database before the core sees the
		// configuration: with a rule-mode binding the generated
		// sub-rules reference GEOIP, and the core refuses to
		// load the configuration at all when the database is
		// missing and its own download fails. DeBrowser
		// downloads and validates it first, so a failure is
		// reported here — clearly, and with nothing started.
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		if err := s.ensureGeoIP(ctx, state.bindings); err != nil {
			return err
		}
		// No separate configuration pre-check: the generated
		// configuration is machine-produced, the core's own load
		// at start is the real check, and a core that fails to
		// load it exits during startup — which Start reports and
		// cleans up. (The binary's -t mode does not parse
		// provider payloads, so it could not validate the parts
		// that matter anyway.)
		if err := s.manager.Start(ctx); err != nil {
			return err
		}
		if err := s.applySelections(ctx); err != nil {
			// The process started but the recorded selections
			// could not be applied: stop the core again, so a
			// start that reports failure never leaves a core
			// running (and no instance can end up on a node
			// other than its recorded one). A failed stop is
			// reported too — claiming the core was stopped when
			// it was not would hide exactly the inconsistency
			// this path exists to prevent.
			if stopErr := s.manager.Stop(); stopErr != nil {
				return fmt.Errorf("核心已启动但实例节点选择应用失败，且停止核心失败（%v），核心可能仍在运行，请在代理页手动关闭核心（选择错误：%w）", stopErr, err)
			}
			return fmt.Errorf("核心已启动但实例节点选择应用失败，已停止核心：%w", err)
		}
		return nil
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
	// rulePolicyOverrides holds only the rulesets the user
	// changed; the generator merges them over the defaults.
	rulePolicyOverrides map[string]string
	customRules         []model.ProxyCustomRule
	settings            model.ProxySettings
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
	policies, err := s.store.ListRulePolicies(ctx)
	if err != nil {
		return proxyConfigState{}, err
	}
	overrides := map[string]string{}
	for _, p := range policies {
		overrides[p.ProviderName] = p.Policy
	}
	customRules, err := s.store.ListCustomRules(ctx)
	if err != nil {
		return proxyConfigState{}, err
	}
	settings, err := s.loadProxySettings(ctx)
	if err != nil {
		return proxyConfigState{}, err
	}
	return proxyConfigState{
		sources: sources, bindings: bindings,
		rulePolicyOverrides: overrides, customRules: customRules,
		settings: settings,
	}, nil
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

// reloadConfig asks the running core to load the configuration
// file at its canonical path.
func (s *ProxyService) reloadConfig(ctx context.Context) error {
	if s.reloadOverride != nil {
		return s.reloadOverride(ctx)
	}
	return s.client.ReloadConfig(ctx, s.configPath())
}

// regenerateState renders the configuration for the given state
// and writes it atomically. When the core is running it
// hot-reloads; a rejected reload restores the previous file and
// leaves the running configuration untouched. If the reload
// succeeds but applying the recorded selections fails, the
// recorded (database) state is regenerated back — file, running
// configuration and selections — so the core never keeps
// serving a configuration the records do not describe; a failed
// rollback is reported as such, with the original failure kept.
// Callers hold s.mu.
func (s *ProxyService) regenerateState(ctx context.Context, state proxyConfigState) error {
	return s.regenerateStateGuarded(ctx, state, false)
}

func (s *ProxyService) regenerateStateGuarded(ctx context.Context, state proxyConfigState, rollingBack bool) error {
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
	// The previous file is the rollback anchor for every
	// failure below: read it BEFORE replacing it. A file that
	// exists but cannot be read aborts the change — proceeding
	// would destroy the only copy of the configuration the core
	// may still be running. A missing file (os.IsNotExist) is
	// recorded as "no previous configuration file" (prev ==
	// nil) and the change proceeds.
	prev, rerr := os.ReadFile(cfgPath)
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("读取现有核心配置失败，已中止本次变更：%w", rerr)
	}
	// restorePrev returns the disk to the pre-change state:
	// with a previous file, write its bytes back; without one,
	// remove the file this call just wrote. Its error is part
	// of the caller's result: a failed restore means the file
	// on disk may no longer match the configuration the core
	// is running, and no caller may claim the previous state
	// was preserved.
	restorePrev := func() error {
		if prev == nil {
			if err := os.Remove(cfgPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		return os.WriteFile(cfgPath, prev, 0o640)
	}
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
		// A reload that introduces a GEOIP reference (the first
		// rule-mode binding appearing while the core runs) needs
		// the database in place first: the core would refuse
		// the new configuration without it and fail an otherwise
		// valid binding change. Preparation failure rolls the
		// file back exactly like a refused reload does.
		if err := s.ensureGeoIP(ctx, state.bindings); err != nil {
			if werr := restorePrev(); werr != nil {
				return fmt.Errorf("GeoIP 数据库准备失败：%w；且原配置恢复失败（%v），磁盘上的配置可能与核心正在运行的配置不一致，请重启代理核心恢复一致性", err, werr)
			}
			return err
		}
		if err := s.reloadConfig(ctx); err != nil {
			// Roll the file back so the next start loads the same
			// configuration the core is still running.
			if werr := restorePrev(); werr != nil {
				return fmt.Errorf("核心拒绝了新配置：%w；且原配置恢复失败（%v），磁盘上的配置可能与核心正在运行的配置不一致，请重启代理核心恢复一致性", err, werr)
			}
			if prev == nil {
				return fmt.Errorf("核心拒绝了新配置，已删除新配置文件（原无配置文件）：%w", err)
			}
			return fmt.Errorf("核心拒绝了新配置，已保留原有配置：%w", err)
		}
		if err := s.applySelections(ctx); err != nil {
			if rollingBack {
				return err
			}
			// The core now runs the NEW configuration while the
			// records still describe the previous state. Restore
			// consistency by regenerating the recorded state:
			// file, running configuration and selections all go
			// back. (For proposed-state flows the records ARE the
			// previous state; for database-truth flows this is a
			// consistency-restoring retry of the same state.)
			prevState, lerr := s.loadConfigState(ctx)
			if lerr != nil {
				return fmt.Errorf("应用节点选择失败：%w；且读取原状态失败（%v），核心配置可能与记录不一致，请重启代理核心恢复一致性", err, lerr)
			}
			if rbErr := s.regenerateStateGuarded(ctx, prevState, true); rbErr != nil {
				return fmt.Errorf("应用节点选择失败：%w；回滚原配置失败（%v），核心配置可能与记录不一致，请重启代理核心恢复一致性", err, rbErr)
			}
			return fmt.Errorf("应用节点选择失败，已回滚到原配置：%w", err)
		}
		return nil
	}
	return nil
}

// buildConfigInput assembles the generator input from a state
// snapshot. Subscription URLs are read from the vault here and
// only here.
func (s *ProxyService) buildConfigInput(ctx context.Context, state proxyConfigState) (mihomo.ConfigInput, error) {
	input := mihomo.ConfigInput{
		ControllerPort: proxyControllerPort, Secret: s.secret,
		LogLevel: state.settings.LogLevel, DoHServers: state.settings.DoHServers,
		ProviderIntervalSec: state.settings.ProviderIntervalSec,
		RulePolicies:        state.rulePolicyOverrides,
	}
	for _, r := range state.customRules {
		input.CustomRules = append(input.CustomRules, mihomo.CustomRuleInput{
			RuleType: r.RuleType, Value: r.Value, Policy: r.Policy, Enabled: r.Enabled,
		})
	}
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
			RouteMode:    b.RouteMode,
		})
	}
	return input, nil
}

// applySelections pushes DeBrowser's recorded instance->node
// selections into the running core's groups. A binding whose node
// currently exists is selected; a binding whose node is gone (or
// whose provider cannot be read) is set to REJECT explicitly, so
// the instance refuses traffic instead of silently falling back
// to another node. The recorded bindings themselves are NEVER
// changed here.
//
// A failed selection is an error, never a warning swallowed on
// the way to a success report: the affected instance's group is
// additionally forced to REJECT (best effort), so a selection
// failure can never leave an instance running on a node other
// than its recorded one, and the caller decides how to surface
// the failure. Other instances' selections still proceed.
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
	var failed []string
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
		if err := s.selectNode(ctx, mihomo.GroupName(b.InstanceID), target); err != nil {
			if target != "REJECT" {
				// Safety first: park the group on the explicit
				// reject target so this instance cannot run on
				// a wrong node, then report the failure.
				_ = s.selectNode(ctx, mihomo.GroupName(b.InstanceID), "REJECT")
			}
			name := b.InstanceID
			if inst, ierr := s.store.GetInstance(ctx, b.InstanceID); ierr == nil && inst.Name != "" {
				name = inst.Name
			}
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("以下实例的节点选择应用失败，其流量已被拒绝（不会改用其他节点）：%s", strings.Join(failed, "、"))
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

// compensateSourceAdd removes what an in-flight source
// addition created, in REFERENCE ORDER: the database row goes
// first, and the vault entry / local file copy it referenced
// is removed only after the row is actually gone — deleting a
// still-referenced credential or file would strand the row.
// It reports whether the row is gone (callers may regenerate
// the configuration only then) and a combined error for every
// step that failed. A step that already reached its goal (row
// absent, file absent) is not a failure. When the error is
// non-nil, residue may remain and the caller must say so
// instead of claiming a clean rollback.
func (s *ProxyService) compensateSourceAdd(ctx context.Context, id, secretRef, fileName string) (bool, error) {
	rowGone := id == ""
	if id != "" {
		if err := s.store.DeleteProxySource(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("删除来源记录失败：%w", err)
		}
		rowGone = true
	}
	var failed []string
	if secretRef != "" && s.vault != nil {
		if _, err := s.vault.DeleteCompensating(secretRef); err != nil {
			failed = append(failed, "删除凭据条目失败："+err.Error())
		}
	}
	if fileName != "" {
		if err := os.Remove(filepath.Join(s.sourcesDir(), fileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, "删除本地文件副本失败："+err.Error())
		}
	}
	if len(failed) > 0 {
		return rowGone, fmt.Errorf("%s", strings.Join(failed, "；"))
	}
	return rowGone, nil
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
		if _, cerr := s.compensateSourceAdd(ctx, "", ref, ""); cerr != nil {
			return SourceView{}, fmt.Errorf("%w；且凭据补偿清理失败（%v），可能残留凭据条目", err, cerr)
		}
		return SourceView{}, err
	}
	src, err := s.store.CreateProxySource(ctx, model.ProxySource{
		ID: id, Name: name, Kind: model.ProxySourceKindURL,
		SecretRef: ref, ProviderName: providerNameFor(id),
	})
	if err != nil {
		if _, cerr := s.compensateSourceAdd(ctx, "", ref, ""); cerr != nil {
			return SourceView{}, fmt.Errorf("保存订阅失败：%w；且凭据补偿清理失败（%v），可能残留凭据条目", err, cerr)
		}
		return SourceView{}, fmt.Errorf("保存订阅失败：%w", err)
	}
	s.mu.Lock()
	if s.coreRunning(ctx) {
		if err := s.regenerate(ctx); err != nil {
			// The core rejected the configuration with the new
			// source: roll the archive back.
			_, cerr := s.compensateSourceAdd(ctx, id, ref, "")
			s.mu.Unlock()
			if cerr != nil {
				return SourceView{}, fmt.Errorf("添加订阅失败，核心配置更新未生效，且回滚清理失败（%v），可能残留来源记录或凭据条目（原始错误：%w）", cerr, err)
			}
			return SourceView{}, fmt.Errorf("添加订阅失败，核心配置更新未生效，已回滚：%w", err)
		}
		// Ask the core to fetch the new provider now. A source
		// only counts as added when the core actually loaded
		// it: a refresh failure rolls the archive back (row,
		// vault entry and the live configuration) instead of
		// reporting success for a source that does not work.
		if err := s.refreshProvider(ctx, src.ProviderName); err != nil {
			// Compensate in reference order; the live
			// configuration is regenerated only when the row
			// is actually gone — regenerating on the
			// assumption of a deleted row while it survives
			// would drop a source the records still describe.
			rowGone, cerr := s.compensateSourceAdd(ctx, id, ref, "")
			var rbErr error
			if rowGone {
				rbErr = s.regenerate(ctx)
			}
			s.mu.Unlock()
			if cerr != nil {
				return SourceView{}, fmt.Errorf("添加订阅失败，核心未能加载该订阅，且回滚清理失败（%v），可能残留来源记录或凭据条目（加载错误：%w）", cerr, err)
			}
			if rbErr != nil {
				return SourceView{}, fmt.Errorf("添加订阅失败，核心未能加载该订阅，且运行配置恢复失败（加载错误：%v；恢复错误：%v），请重启核心以恢复一致", err, rbErr)
			}
			return SourceView{}, fmt.Errorf("添加订阅失败，核心未能加载该订阅，已回滚：%w", err)
		}
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
		if _, cerr := s.compensateSourceAdd(ctx, "", "", fileName); cerr != nil {
			return SourceView{}, fmt.Errorf("保存本地来源失败：%w；且本地文件补偿清理失败（%v），可能残留文件副本", err, cerr)
		}
		return SourceView{}, fmt.Errorf("保存本地来源失败：%w", err)
	}
	s.mu.Lock()
	if s.coreRunning(ctx) {
		if err := s.regenerate(ctx); err != nil {
			_, cerr := s.compensateSourceAdd(ctx, id, "", fileName)
			s.mu.Unlock()
			if cerr != nil {
				return SourceView{}, fmt.Errorf("导入文件失败，核心配置更新未生效，且回滚清理失败（%v），可能残留来源记录或文件副本（原始错误：%w）", cerr, err)
			}
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
	logFile, err := os.Create(filepath.Join(dir, "validate.log"))
	if err != nil {
		return 0, fmt.Errorf("创建验证日志失败：%w", err)
	}
	defer logFile.Close()
	// The throwaway core runs through the same child-process
	// management as the production core (on Windows: the same
	// kill-on-close Job Object), and it is ALWAYS terminated and
	// reaped before this function returns — on success, on
	// failure, on timeout and on context cancellation alike.
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	child, err := mihomo.StartProcess(bin, []string{"-d", dir, "-f", cfgPath}, dir, logFile)
	if err != nil {
		return 0, fmt.Errorf("启动验证核心失败：%w", err)
	}
	done := make(chan struct{})
	go func() {
		_ = child.Wait()
		close(done)
	}()
	defer func() {
		_ = child.Kill()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()
	client := mihomo.NewClient(proxyValidatePort, s.secret)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := client.Version(vctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("验证核心未能启动，无法验证文件内容")
		}
		select {
		case <-vctx.Done():
			return 0, vctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	// The provider's initial load completes with startup; poll
	// briefly so a slow first parse is not misread as empty.
	deadline = time.Now().Add(5 * time.Second)
	for {
		nodes, err := client.ProviderProxies(vctx, "candidate")
		if err == nil && len(nodes) > 0 {
			return len(nodes), nil
		}
		if time.Now().After(deadline) {
			return 0, nil
		}
		select {
		case <-vctx.Done():
			return 0, vctx.Err()
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
		if err := s.refreshProvider(ctx, src.ProviderName); err != nil {
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
	// Keep the previous content: if the running core refuses the
	// new file at refresh time, the old file is restored and
	// reloaded, so the disk copy and the running configuration
	// never disagree.
	oldData, _ := os.ReadFile(filepath.Join(s.sourcesDir(), src.FileName))
	if err := s.writeSourceCopy(src.FileName, data); err != nil {
		return SourceView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.coreRunning(ctx) {
		if err := s.refreshProvider(ctx, src.ProviderName); err != nil {
			var rollbackErr error
			switch {
			case oldData == nil:
				rollbackErr = fmt.Errorf("原文件内容不可用")
			default:
				if werr := s.writeSourceCopy(src.FileName, oldData); werr != nil {
					rollbackErr = werr
				} else if rerr := s.refreshProvider(ctx, src.ProviderName); rerr != nil {
					rollbackErr = rerr
				}
			}
			if rollbackErr != nil {
				return SourceView{}, fmt.Errorf("新文件刷新失败，且原文件恢复失败（刷新错误：%v；恢复错误：%v），请重新导入原文件", err, rollbackErr)
			}
			if selErr := s.applySelections(ctx); selErr != nil {
				return SourceView{}, fmt.Errorf("新文件刷新失败，已恢复原文件，但节点选择恢复失败（%v），请重启核心以恢复一致（刷新错误：%w）", selErr, err)
			}
			return SourceView{}, fmt.Errorf("新文件刷新失败，已恢复原文件和原有节点：%w", err)
		}
		if err := s.applySelections(ctx); err != nil {
			return SourceView{}, err
		}
	}
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
			// The runtime configuration was already switched to
			// the proposed state; restore it so database and
			// core agree again. A failed restore is reported,
			// never hidden.
			if rerr := s.regenerate(ctx); rerr != nil {
				return fmt.Errorf("删除来源失败，且运行配置恢复失败（删除错误：%v；恢复错误：%v），请重启核心以恢复一致", err, rerr)
			}
			return fmt.Errorf("删除来源失败，运行配置已恢复：%w", err)
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
				Name: n.Name, Type: n.Type, Alive: n.Alive, Delay: n.Delay, Checked: n.Checked,
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
	settings, err := s.loadProxySettings(ctx)
	if err != nil {
		return nil, err
	}
	out := []NodeDelayView{}
	for _, src := range sources {
		nodes, err := s.client.ProviderProxies(ctx, src.ProviderName)
		if err != nil {
			continue
		}
		delays, err := s.client.GroupDelay(ctx, mihomo.DelayGroupName(src.ProviderName), settings.TestURL, settings.TestTimeoutMs)
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

// nodeState derives a binding's node health display from the
// core's own data — existence from the source's current node
// list, health from the core's health-check history:
//
//   - "missing": the node is gone from its source (yellow);
//   - "down": the node exists but its latest health check
//     failed (red);
//   - "ok": the node exists and its latest health check
//     passed (green);
//   - "unknown": not yet health-checked, or the core is
//     stopped — never presented as healthy (neutral).
//
// The state is display-only; it never changes the binding.
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
			if !n.Checked {
				return "unknown"
			}
			if n.Alive {
				return "ok"
			}
			return "down"
		}
	}
	return "missing"
}

func (s *ProxyService) bindingView(ctx context.Context, b model.InstanceProxyBinding) BindingView {
	view := BindingView{
		InstanceID: b.InstanceID, SourceID: b.SourceID, NodeName: b.NodeName,
		Port: b.Port, RouteMode: b.RouteMode, NodeState: "unknown", ListenerState: "unknown",
	}
	if view.RouteMode == "" {
		view.RouteMode = model.RouteModeRule
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
	if s.InstanceStatus != nil {
		if st, known := s.InstanceStatus(b.InstanceID); known {
			view.InstanceRunning = st == string(model.StatusRunning) || st == string(model.StatusUnknown)
		}
	}
	if s.coreRunning(ctx) {
		// ProbeListener (a SOCKS5 greeting), not a bare TCP dial:
		// when a non-proxy program occupies the port, the core's
		// listener never came up but the port still "answers" —
		// the greeting separates a live proxy listener from a
		// mere occupant (a protocol probe; see ProbeListener for
		// the ownership boundary).
		if mihomo.ProbeListener(b.Port) {
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
	routeMode := model.RouteModeRule
	if existing, err := s.store.GetBinding(ctx, instanceID); err == nil {
		port = existing.Port // an instance keeps its listener port across changes
		if existing.RouteMode != "" {
			routeMode = existing.RouteMode // and its routing mode: re-binding a node never resets it
		}
	} else {
		p, err := s.allocatePort(ctx)
		if err != nil {
			return BindingView{}, err
		}
		port = p
	}
	proposed := model.InstanceProxyBinding{
		InstanceID: instanceID, SourceID: sourceID, NodeName: nodeName, Port: port,
		RouteMode: routeMode,
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
		if rerr := s.regenerate(ctx); rerr != nil {
			return BindingView{}, fmt.Errorf("保存代理绑定失败，且运行配置恢复失败（保存错误：%v；恢复错误：%v），请重启核心以恢复一致", err, rerr)
		}
		return BindingView{}, fmt.Errorf("保存代理绑定失败，运行配置已恢复：%w", err)
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
	settings, err := s.loadProxySettings(ctx)
	if err != nil {
		return 0, err
	}
	taken := map[int]bool{}
	for _, p := range used {
		taken[p] = true
	}
	// The scan range starts at the configured listener port
	// start (an editable setting) and spans 1000 ports. Existing
	// bindings keep their recorded ports wherever they are —
	// the setting only steers NEW allocations.
	start := settings.ListenerPortStart
	end := start + 999
	if end > 65535 {
		end = 65535
	}
	for p := start; p <= end; p++ {
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
			if rerr := s.regenerate(ctx); rerr != nil {
				return fmt.Errorf("清除代理绑定失败，且运行配置恢复失败（清除错误：%v；恢复错误：%v），请重启核心以恢复一致", err, rerr)
			}
			return fmt.Errorf("清除代理绑定失败，运行配置已恢复：%w", err)
		}
		return nil
	}()
	finishOp(op, err)
	return err
}

// SetRouteMode changes one bound instance's routing mode
// (rule | global | direct). The mode is part of the binding
// ReassignInstancePort moves one instance's binding to a fresh
// free listener port — the one-click recovery when the recorded
// port is occupied by another program. The core never takes an
// occupied port over; the binding moves instead. The operation
// refuses while the instance's browser may be running: that
// process still points at the old --proxy-server port.
//
// With the core stopped only the stored port changes. With the
// core running the change follows the proposed-config-first
// discipline: regenerate + hot reload + applySelections, then
// the NEW listener must answer a SOCKS5 greeting (proving a
// proxy listener came up on the fresh port, which was verified
// free immediately before the reload) before the
// new port is persisted; any failure regenerates the previous
// state back and keeps the old port.
func (s *ProxyService) ReassignInstancePort(operationID string, instanceID string) (BindingView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_reassign_port", "browser_instance", instanceID)
	view, err := s.reassignInstancePort(instanceID)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) reassignInstancePort(instanceID string) (BindingView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The instance lifecycle lock comes FIRST and is held for
	// the whole operation: the status check below and every
	// state change after it are atomic against instance start,
	// stop and deletion, which hold the same lock (see
	// instance_lifecycle.go). Lock order note: this lock is
	// always taken before s.mu — the launch gate inside a start
	// takes s.mu (selection heal) while holding it, never the
	// other way around.
	if s.Lifecycle != nil {
		unlock := s.Lifecycle.Lock(instanceID)
		defer unlock()
	}
	// Only a confirmed stopped instance may change ports: a
	// starting/running/stopping browser still points (or is
	// about to point) at the old --proxy-server port, and an
	// unconfirmed state cannot prove otherwise.
	if s.InstanceStatus != nil {
		st, known := s.InstanceStatus(instanceID)
		if !known || st != string(model.StatusStopped) {
			return BindingView{}, fmt.Errorf("实例尚未停止，请先停止实例后再更换端口")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := s.store.GetBinding(ctx, instanceID)
	if errors.Is(err, store.ErrNotFound) {
		return BindingView{}, fmt.Errorf("该实例尚未绑定代理节点，无需更换端口")
	}
	if err != nil {
		return BindingView{}, err
	}
	// allocatePort skips every recorded binding port (including
	// this binding's current one) and every port that answers a
	// TCP dial (including a squatter's), so the pick is free on
	// both counts.
	newPort, err := s.allocatePort(ctx)
	if err != nil {
		return BindingView{}, err
	}

	if !s.coreRunning(ctx) {
		if err := s.store.UpdateBindingPort(ctx, instanceID, newPort); err != nil {
			return BindingView{}, err
		}
		b.Port = newPort
		return s.bindingView(ctx, b), nil
	}

	state, err := s.loadConfigState(ctx)
	if err != nil {
		return BindingView{}, err
	}
	previous := state
	proposed := state
	proposed.bindings = make([]model.InstanceProxyBinding, len(state.bindings))
	copy(proposed.bindings, state.bindings)
	for i := range proposed.bindings {
		if proposed.bindings[i].InstanceID == instanceID {
			proposed.bindings[i].Port = newPort
		}
	}
	if err := s.regenerateState(ctx, proposed); err != nil {
		return BindingView{}, fmt.Errorf("核心配置更新失败，端口未更换：%w", err)
	}
	if !probeListenerWait(newPort) {
		if rbErr := s.regenerateState(ctx, previous); rbErr != nil {
			return BindingView{}, fmt.Errorf("新端口 %d 的监听未能就绪，且原配置恢复失败（%v），核心配置可能与记录不一致，请重启代理核心恢复一致性", newPort, rbErr)
		}
		return BindingView{}, fmt.Errorf("新端口 %d 的监听未能就绪，已恢复原端口 %d，请重试", newPort, b.Port)
	}
	if err := s.store.UpdateBindingPort(ctx, instanceID, newPort); err != nil {
		if rbErr := s.regenerateState(ctx, previous); rbErr != nil {
			return BindingView{}, fmt.Errorf("保存新端口失败且配置回滚失败：%v（保存错误：%w）", rbErr, err)
		}
		return BindingView{}, fmt.Errorf("保存新端口失败，端口未更换：%w", err)
	}
	b.Port = newPort
	return s.bindingView(ctx, b), nil
}

// probeListenerWait polls the SOCKS5-greeting listener probe
// for a few seconds: a listener created by a hot reload may
// need a moment to come up.
func probeListenerWait(port int) bool {
	for i := 0; i < 12; i++ {
		if mihomo.ProbeListener(port) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// record; the generated configuration derives the instance's
// sub-rules from it. Same consistency discipline as the other
// binding mutations: the proposed configuration is applied
// first (hot-reloaded into a running core, selections
// re-applied), the record is written only after the core
// accepted it, and a failed record write restores the previous
// configuration.
func (s *ProxyService) SetRouteMode(operationID string, instanceID string, routeMode string) (BindingView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.binding_route_mode", "browser_instance", instanceID)
	view, err := s.setRouteMode(instanceID, routeMode)
	finishOp(op, err)
	return view, err
}

func (s *ProxyService) setRouteMode(instanceID string, routeMode string) (BindingView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch routeMode {
	case model.RouteModeRule, model.RouteModeGlobal, model.RouteModeDirect:
	default:
		return BindingView{}, fmt.Errorf("无效的路由模式：%s", routeMode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.store.GetBinding(ctx, instanceID)
	if errors.Is(err, store.ErrNotFound) {
		return BindingView{}, fmt.Errorf("该实例尚未绑定代理节点，无法设置路由模式")
	}
	if err != nil {
		return BindingView{}, err
	}
	if b.RouteMode == routeMode {
		return s.bindingView(ctx, b), nil
	}
	state, err := s.loadConfigState(ctx)
	if err != nil {
		return BindingView{}, err
	}
	for i := range state.bindings {
		if state.bindings[i].InstanceID == instanceID {
			state.bindings[i].RouteMode = routeMode
		}
	}
	if err := s.regenerateState(ctx, state); err != nil {
		return BindingView{}, fmt.Errorf("核心配置更新失败，路由模式未修改：%w", err)
	}
	if err := s.store.UpdateBindingRouteMode(ctx, instanceID, routeMode); err != nil {
		if rerr := s.regenerate(ctx); rerr != nil {
			return BindingView{}, fmt.Errorf("路由模式保存失败，且运行配置恢复失败（保存错误：%v；恢复错误：%v），请重启核心以恢复一致", err, rerr)
		}
		return BindingView{}, fmt.Errorf("路由模式保存失败，运行配置已恢复：%w", err)
	}
	b.RouteMode = routeMode
	return s.bindingView(ctx, b), nil
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
	if err := s.healRejectedGroup(ctx, b); err != nil {
		return 0, err
	}
	if !mihomo.ProbeListener(b.Port) {
		return 0, fmt.Errorf("该实例的代理监听端口 127.0.0.1:%d 无响应（可能被其他程序占用），实例未启动。可在「代理配置」页面为该实例重分配空闲端口后重试", b.Port)
	}
	return b.Port, nil
}

// healRejectedGroup is the launch gate's one-shot self-healing
// for an instance group the core has parked on REJECT. The core
// refreshes http providers on its own schedule (interval 86400),
// outside DeBrowser's operations: a node that disappears and
// returns inside that cycle leaves its group on REJECT even
// though the recorded binding is servable again, because
// applySelections normally runs only on DeBrowser-side
// operations. Instead of polling in the background (there is no
// watchdog, by design), the launch gate checks the group's
// current selection: REJECT triggers exactly one re-application
// of the recorded selections and a re-check.
//
// Outcomes: the group serves its bound node (already, or healed)
// → nil and the launch proceeds. Still REJECT after re-applying
// → the bound node is genuinely unavailable in its source right
// now, and the launch is refused with a clear message instead of
// starting a browser whose traffic the core would only reject.
// A selection read that fails outright (a core hiccup) does not
// block the launch: the listener probe in the caller remains the
// authority, exactly as before this check existed.
func (s *ProxyService) healRejectedGroup(ctx context.Context, b model.InstanceProxyBinding) error {
	now, err := s.groupNow(ctx, mihomo.GroupName(b.InstanceID))
	if err != nil || now != "REJECT" {
		return nil
	}
	hctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.coreRunning(hctx) {
		return nil // raced with a core stop; the gate's own checks decide
	}
	return s.reapplySelectionForBinding(hctx, b)
}

// reapplySelectionForBinding re-applies the recorded selections
// once and reports whether the binding's group now serves its
// recorded node. Callers hold s.mu and have established that the
// core is running.
func (s *ProxyService) reapplySelectionForBinding(ctx context.Context, b model.InstanceProxyBinding) error {
	applyErr := s.applySelections(ctx)
	now, err := s.groupNow(ctx, mihomo.GroupName(b.InstanceID))
	if err == nil && now == b.NodeName {
		return nil
	}
	if applyErr != nil {
		return fmt.Errorf("该实例绑定的代理节点「%s」当前无法应用到核心，实例未启动：%w", b.NodeName, applyErr)
	}
	return fmt.Errorf("该实例绑定的代理节点「%s」已不在其来源中或当前不可用，实例未启动。请在「代理配置」页面确认节点状态，或为该实例更换节点", b.NodeName)
}

// RefreshAfterInstanceDeleted is the deletion hook the instance
// service calls after an instance record is gone (its binding row
// went with it, via the schema cascade). A running core would
// otherwise keep serving the deleted instance's listener and
// group until the next unrelated reload or a core restart;
// regenerating here drops them immediately, and the regeneration
// re-applies the surviving instances' selections. A stopped core
// needs nothing: the next start generates from the database
// anyway. Failures are logged, never returned — the deletion has
// already committed, and the residue degrades to the pre-hook
// behaviour (cleaned up by the next successful regeneration).
func (s *ProxyService) RefreshAfterInstanceDeleted(instanceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.coreRunning(ctx) {
		return
	}
	if err := s.regenerate(ctx); err != nil {
		s.log.Warn("proxy configuration refresh after instance deletion failed; the deleted instance's listener is removed on the next reload or core restart",
			"instance", instanceID, "error", err)
	}
}
