package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// ProxyService is the proxy-management service exposed to the
// frontend over Wails bindings (Stage 4A). It owns the vault ↔
// database consistency protocol reviewed for the secrets store:
//
//   - Create: vault Put first (durable), then the database
//     transaction that publishes the reference; a failed
//     transaction is compensated with DeleteCompensating, which
//     can only ever remove the still-pending ref created here.
//   - Update with new secrets: Put a NEW ref, swap it in one
//     transaction, and only then clean up the old ref.
//   - Delete / clear: the database reference is removed first,
//     then the vault entry is cleaned with DeleteIfUnreferenced.
//   - No unconditional vault deletion exists anywhere in this
//     service. Cleanup failures leave identifiable orphans for
//     the age-gated garbage collection; they never fail an
//     otherwise committed operation and never dangle a reference.
//
// Every ref-creating or ref-dropping database mutation and every
// vault cleanup in this service is serialised by s.mu, as the
// secrets store contract requires: the in-use check inside
// DeleteIfUnreferenced runs under the vault lock, and the service
// mutex makes it atomic with respect to reference changes.
//
// Credential material never leaves the vault: views report only
// whether secrets exist, their field names and a status. A proxy
// whose credential cannot be loaded is reported as
// credential-unavailable — it is NEVER silently treated as a
// credential-less proxy (fail closed).
type ProxyService struct {
	store *store.Store
	vault *secrets.Store
	log   *slog.Logger
	mu    sync.Mutex
	// dialContext performs the TCP dial for TestProxy. It is a
	// field so the default dialer is explicit; production never
	// replaces it.
	dialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// withTx runs the service's database transactions. It is a
	// field so tests can inject a transaction failure after a
	// successful vault Put and verify the compensating delete;
	// production always uses the store's real WithTx.
	withTx func(ctx context.Context, fn func(tx *store.Store) error) error
}

// NewProxyService builds the service over the database and the
// already-open credential vault, then runs one garbage-collection
// pass (best effort: failures are logged, never fatal — the next
// pass retries). The database and vault handles are owned by the
// caller and are never closed here.
func NewProxyService(db *database.DB, vault *secrets.Store, log *slog.Logger) (*ProxyService, error) {
	if vault == nil {
		return nil, fmt.Errorf("proxy service: credential vault is required")
	}
	st := store.New(db)
	s := &ProxyService{store: st, vault: vault, log: log}
	s.dialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	s.withTx = st.WithTx
	if n, err := s.CollectSecretGarbage(); err != nil {
		log.Warn("startup credential garbage collection failed", "error", err)
	} else if n > 0 {
		log.Info("startup credential garbage collection removed orphans", "count", n)
	}
	return s, nil
}

// Credential status values reported in ProxyView.
const (
	CredentialNone        = "none"        // no credential stored
	CredentialOK          = "ok"          // credential loads normally
	CredentialUnavailable = "unavailable" // ref exists but cannot be loaded
)

// ProxyView is the credential-free projection of a proxy used by
// every read path of this service. It deliberately has no field
// that could carry a secret value or the vault reference itself.
type ProxyView struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Protocol         string   `json:"protocol"`
	Address          string   `json:"address"`
	Port             *int     `json:"port,omitempty"`
	ConfigJSON       string   `json:"configJson"`
	Enabled          bool     `json:"enabled"`
	SortOrder        int      `json:"sortOrder"`
	Source           string   `json:"source"`
	SubscriptionID   *string  `json:"subscriptionId,omitempty"`
	HasSecrets       bool     `json:"hasSecrets"`
	SecretFields     []string `json:"secretFields"`
	CredentialStatus string   `json:"credentialStatus"`
	LastLatencyMs    *int     `json:"lastLatencyMs,omitempty"`
	LastTestOK       *bool    `json:"lastTestOk,omitempty"`
	LastTestedAt     *string  `json:"lastTestedAt,omitempty"`
	CreatedAt        string   `json:"createdAt"`
	UpdatedAt        string   `json:"updatedAt"`
}

// CreateProxyInput carries the user-editable fields for creation.
// Secrets maps credential field names (for example "password",
// "uuid") to plaintext values; the values are written to the
// credential vault only and are never stored in the database,
// logged or returned.
type CreateProxyInput struct {
	Name       string            `json:"name"`
	Protocol   string            `json:"protocol"`
	Address    string            `json:"address"`
	Port       *int              `json:"port"`
	ConfigJSON string            `json:"configJson"`
	SortOrder  int               `json:"sortOrder"`
	Secrets    map[string]string `json:"secrets"`
}

// UpdateProxyInput carries the editable fields of one proxy.
// Secret semantics (reviewed design): a nil or empty Secrets map
// keeps the stored credential untouched; a non-empty map replaces
// it wholesale; ClearSecrets explicitly removes it (and is ignored
// when Secrets is non-empty).
type UpdateProxyInput struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Protocol     string            `json:"protocol"`
	Address      string            `json:"address"`
	Port         *int              `json:"port"`
	ConfigJSON   string            `json:"configJson"`
	SortOrder    int               `json:"sortOrder"`
	Enabled      bool              `json:"enabled"`
	Secrets      map[string]string `json:"secrets"`
	ClearSecrets bool              `json:"clearSecrets"`
}

// TestProxyResult reports one connection test. The test is a TCP
// reachability check against the node's server endpoint (the only
// honest test available before the Mihomo core lands (Stage 7);
// protocol-level delay and exit-IP tests arrive there).
type TestProxyResult struct {
	Proxy     ProxyView `json:"proxy"`
	OK        bool      `json:"ok"`
	LatencyMs *int      `json:"latencyMs,omitempty"`
	Message   string    `json:"message"`
}

// ProxyPortRange is the instance listener port range (Stage 7
// allocates from it). Stored in app_settings as 'proxy.port_range'
// ("19000-19499"); the reviewed default applies when unset or
// invalid.
type ProxyPortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

const (
	portRangeSettingKey = "proxy.port_range"
	defaultPortStart    = 19000
	defaultPortEnd      = 19499
)

var proxyProtocols = map[string]bool{
	"http": true, "https": true, "socks5": true,
	"vmess": true, "vless": true, "trojan": true, "shadowsocks": true,
	"hysteria2": true, "tuic": true, "wireguard": true, "direct": true,
}

// secretConfigKeys are field names that must never appear in the
// non-secret config_json: they belong in the credential vault.
var secretConfigKeys = map[string]bool{
	"password": true, "passwd": true, "pwd": true,
	"uuid":        true,
	"private-key": true, "private_key": true, "privatekey": true,
	"preshared-key": true, "preshared_key": true, "psk": true,
	"obfs-password": true, "obfs_password": true,
	"token": true,
}

// ----------------------------------------------------------------
// Read paths
// ----------------------------------------------------------------

// ListProxies returns every proxy as a credential-free view, in
// display order.
func (s *ProxyService) ListProxies() ([]ProxyView, error) {
	c, cancel := ctx()
	defer cancel()
	list, err := s.store.ListProxies(c)
	if err != nil {
		return nil, err
	}
	out := make([]ProxyView, 0, len(list))
	for _, p := range list {
		out = append(out, s.toView(p))
	}
	return out, nil
}

// GetProxy returns one proxy as a credential-free view.
func (s *ProxyService) GetProxy(id string) (ProxyView, error) {
	c, cancel := ctx()
	defer cancel()
	p, err := s.store.GetProxy(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ProxyView{}, fmt.Errorf("代理不存在")
		}
		return ProxyView{}, err
	}
	return s.toView(p), nil
}

// toView projects a stored proxy into its credential-free view,
// resolving the credential status against the vault. A reference
// that cannot be loaded yields CredentialUnavailable — never a
// silent downgrade to "no credential".
func (s *ProxyService) toView(p model.Proxy) ProxyView {
	v := ProxyView{
		ID: p.ID, Name: p.Name, Protocol: p.Protocol, Address: p.Address,
		Port: p.Port, ConfigJSON: p.ConfigJSON, Enabled: p.Enabled,
		SortOrder: p.SortOrder, Source: p.Source,
		SubscriptionID: p.SubscriptionID,
		LastLatencyMs:  p.LastLatencyMs, LastTestOK: p.LastTestOK,
		LastTestedAt: p.LastTestedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		SecretFields: []string{},
	}
	if p.SecretRef == "" {
		v.CredentialStatus = CredentialNone
		return v
	}
	v.HasSecrets = true
	plain, err := s.vault.Get(p.SecretRef)
	if err != nil {
		v.CredentialStatus = CredentialUnavailable
		return v
	}
	var envelope map[string]string
	if err := json.Unmarshal(plain, &envelope); err != nil {
		v.CredentialStatus = CredentialUnavailable
		return v
	}
	fields := make([]string, 0, len(envelope))
	for k := range envelope {
		fields = append(fields, k)
	}
	sort.Strings(fields)
	v.SecretFields = fields
	v.CredentialStatus = CredentialOK
	return v
}

// ----------------------------------------------------------------
// Proxy write paths (vault consistency protocol; all under s.mu)
// ----------------------------------------------------------------

// CreateProxy validates and creates a proxy. With secrets, the
// vault write lands first and the database transaction second;
// see the type comment for the full protocol.
func (s *ProxyService) CreateProxy(input CreateProxyInput) (ProxyView, error) {
	fields, err := validateProxyFields(input.Name, input.Protocol, input.Address, input.Port, input.ConfigJSON)
	if err != nil {
		return ProxyView{}, err
	}
	if err := validateSecrets(input.Secrets); err != nil {
		return ProxyView{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, cancel := ctx()
	defer cancel()

	ref := ""
	if len(input.Secrets) > 0 {
		ref, err = s.putEnvelope(input.Secrets)
		if err != nil {
			return ProxyView{}, err
		}
	}
	row := model.Proxy{
		Name: fields.name, Protocol: fields.protocol, Address: fields.address,
		Port: fields.port, ConfigJSON: fields.configJSON, SecretRef: ref,
		Enabled: true, SortOrder: input.SortOrder,
		Source: "manual",
	}
	var created model.Proxy
	txErr := s.withTx(c, func(tx *store.Store) error {
		var err error
		created, err = tx.CreateProxy(c, row)
		return err
	})
	if txErr != nil {
		// The database never published the ref: compensate with
		// the pending-only delete. A compensation failure leaves
		// an orphan for GC and is logged, never hidden.
		if ref != "" {
			if _, cerr := s.vault.DeleteCompensating(ref); cerr != nil {
				s.log.Warn("compensating vault delete failed after create rollback; orphan left for GC",
					"ref", ref, "error", cerr)
			}
		}
		return ProxyView{}, mapProxyConstraintError(txErr)
	}
	if ref != "" {
		s.vault.MarkCommitted(ref)
	}
	return s.toView(created), nil
}

// UpdateProxy validates and updates a proxy, applying the secret
// semantics documented on UpdateProxyInput.
func (s *ProxyService) UpdateProxy(input UpdateProxyInput) (ProxyView, error) {
	fields, err := validateProxyFields(input.Name, input.Protocol, input.Address, input.Port, input.ConfigJSON)
	if err != nil {
		return ProxyView{}, err
	}
	if err := validateSecrets(input.Secrets); err != nil {
		return ProxyView{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	current, err := s.store.GetProxy(c, input.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ProxyView{}, fmt.Errorf("代理不存在")
		}
		return ProxyView{}, err
	}

	// Secret decision: replace (Put new first), clear, or keep.
	newRef := ""
	if len(input.Secrets) > 0 {
		newRef, err = s.putEnvelope(input.Secrets)
		if err != nil {
			return ProxyView{}, err
		}
	}
	row := current
	row.Name = fields.name
	row.Protocol = fields.protocol
	row.Address = fields.address
	row.Port = fields.port
	row.ConfigJSON = fields.configJSON
	row.Enabled = input.Enabled
	row.SortOrder = input.SortOrder
	switch {
	case newRef != "":
		row.SecretRef = newRef
	case input.ClearSecrets:
		row.SecretRef = ""
	}
	var updated model.Proxy
	txErr := s.withTx(c, func(tx *store.Store) error {
		var err error
		updated, err = tx.UpdateProxy(c, row)
		return err
	})
	if txErr != nil {
		if newRef != "" {
			if _, cerr := s.vault.DeleteCompensating(newRef); cerr != nil {
				s.log.Warn("compensating vault delete failed after update rollback; orphan left for GC",
					"ref", newRef, "error", cerr)
			}
		}
		return ProxyView{}, mapProxyConstraintError(txErr)
	}
	if newRef != "" {
		s.vault.MarkCommitted(newRef)
	}
	// The transaction committed: the old ref may now be cleaned
	// up — guarded by the live-reference check, and only when it
	// actually changed. A cleanup failure leaves an orphan for
	// GC; the update itself already succeeded.
	oldRef := current.SecretRef
	if oldRef != "" && oldRef != row.SecretRef {
		s.cleanupRef(oldRef, "update")
	}
	return s.toView(updated), nil
}

// DeleteProxy removes a proxy: the database row goes first
// (referencing instances get proxy_id NULL via the schema), then
// the vault entry is cleaned under the live-reference guard.
func (s *ProxyService) DeleteProxy(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	current, err := s.store.GetProxy(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("代理不存在")
		}
		return err
	}
	if err := s.store.DeleteProxy(c, id); err != nil {
		return err
	}
	if current.SecretRef != "" {
		s.cleanupRef(current.SecretRef, "delete")
	}
	s.log.Info("proxy deleted", "id", id, "name", current.Name)
	return nil
}

// SetProxyEnabled toggles a proxy's enabled flag. It touches no
// credential reference, so it needs no vault coordination.
func (s *ProxyService) SetProxyEnabled(id string, enabled bool) (ProxyView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	current, err := s.store.GetProxy(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ProxyView{}, fmt.Errorf("代理不存在")
		}
		return ProxyView{}, err
	}
	current.Enabled = enabled
	if _, err := s.store.UpdateProxy(c, current); err != nil {
		return ProxyView{}, err
	}
	return s.GetProxy(id)
}

// cleanupRef removes a vault entry that database rows may no
// longer reference, using the guarded delete: the in-use check
// runs under the vault lock while s.mu serialises reference
// changes, so a still-shared ref is never removed. Failures are
// logged and leave an orphan for GC; they never fail the caller's
// already-committed operation.
func (s *ProxyService) cleanupRef(ref, op string) {
	deleted, err := s.vault.DeleteIfUnreferenced(ref, func(r string) (bool, error) {
		cc, cancel := ctx()
		defer cancel()
		return s.store.SecretRefInUse(cc, r)
	})
	if err != nil {
		s.log.Warn("vault cleanup after "+op+" failed; orphan left for GC", "ref", ref, "error", err)
		return
	}
	if !deleted {
		s.log.Info("vault entry kept: still referenced by another record", "ref", ref, "op", op)
	}
}

// putEnvelope serialises a secrets map and stores it in the vault,
// returning the new (still pending) reference.
func (s *ProxyService) putEnvelope(secrets map[string]string) (string, error) {
	raw, err := json.Marshal(secrets)
	if err != nil {
		return "", fmt.Errorf("编码凭据失败：%w", err)
	}
	ref, err := s.vault.Put(raw)
	if err != nil {
		return "", fmt.Errorf("保存凭据失败：%w", err)
	}
	return ref, nil
}

// CollectSecretGarbage removes vault entries that no database row
// references and that are older than the vault's 24-hour safety
// window. It returns how many entries were removed.
func (s *ProxyService) CollectSecretGarbage() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	live, err := s.store.AllSecretRefs(c)
	if err != nil {
		return 0, err
	}
	removed, err := s.vault.CollectGarbage(live, time.Now())
	if err != nil {
		return 0, fmt.Errorf("清理孤儿凭据失败：%w", err)
	}
	return len(removed), nil
}

// ----------------------------------------------------------------
// Connection test
// ----------------------------------------------------------------

// TestProxy runs the TCP reachability test described on
// TestProxyResult and records the outcome on the proxy row.
func (s *ProxyService) TestProxy(id string) (TestProxyResult, error) {
	c, cancel := ctx()
	defer cancel()
	p, err := s.store.GetProxy(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return TestProxyResult{}, fmt.Errorf("代理不存在")
		}
		return TestProxyResult{}, err
	}
	if p.Protocol == "direct" {
		return TestProxyResult{}, fmt.Errorf("直连节点不经过代理服务器，无需连接测试")
	}
	if p.Address == "" || p.Port == nil {
		return TestProxyResult{}, fmt.Errorf("该节点缺少服务器地址或端口，无法进行连接测试")
	}
	target := net.JoinHostPort(p.Address, strconv.Itoa(*p.Port))
	start := time.Now()
	conn, derr := s.dialContext(c, "tcp", target)
	elapsed := time.Since(start)
	testedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if derr != nil {
		if err := s.store.UpdateProxyTestResult(c, id, nil, false, testedAt); err != nil {
			return TestProxyResult{}, err
		}
		view, err := s.GetProxy(id)
		if err != nil {
			return TestProxyResult{}, err
		}
		return TestProxyResult{
			Proxy: view, OK: false,
			Message: fmt.Sprintf("连接失败：无法连接 %s（%v）", target, derr),
		}, nil
	}
	_ = conn.Close()
	latency := int(elapsed.Milliseconds())
	if err := s.store.UpdateProxyTestResult(c, id, &latency, true, testedAt); err != nil {
		return TestProxyResult{}, err
	}
	view, err := s.GetProxy(id)
	if err != nil {
		return TestProxyResult{}, err
	}
	return TestProxyResult{
		Proxy: view, OK: true, LatencyMs: &latency,
		Message: fmt.Sprintf("连接成功：%s 可达，延迟 %d ms", target, latency),
	}, nil
}

// ----------------------------------------------------------------
// Port range (Stage 7 groundwork)
// ----------------------------------------------------------------

// GetProxyPortRange resolves the instance listener port range
// from app_settings, falling back to the reviewed default
// 19000-19499 when the key is unset or malformed.
func (s *ProxyService) GetProxyPortRange() (ProxyPortRange, error) {
	c, cancel := ctx()
	defer cancel()
	def := ProxyPortRange{Start: defaultPortStart, End: defaultPortEnd}
	raw, err := s.store.GetSetting(c, portRangeSettingKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return def, nil
		}
		return ProxyPortRange{}, err
	}
	startStr, endStr, ok := strings.Cut(raw, "-")
	if !ok {
		return def, nil
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(startStr))
	end, err2 := strconv.Atoi(strings.TrimSpace(endStr))
	if err1 != nil || err2 != nil || start < 1 || end > 65535 || start > end {
		return def, nil
	}
	return ProxyPortRange{Start: start, End: end}, nil
}

// ----------------------------------------------------------------
// Validation
// ----------------------------------------------------------------

type validatedFields struct {
	name       string
	protocol   string
	address    string
	port       *int
	configJSON string
}

// validateProxyFields applies the business rules for the editable
// proxy fields: a known protocol, an endpoint for every protocol
// that has one, and a config_json that is a JSON object free of
// credential fields.
func validateProxyFields(name, protocol, address string, port *int, configJSON string) (validatedFields, error) {
	out := validatedFields{
		name: strings.TrimSpace(name), protocol: strings.TrimSpace(protocol),
		address: strings.TrimSpace(address), port: port,
	}
	if out.name == "" {
		return out, fmt.Errorf("代理名称不能为空")
	}
	if !proxyProtocols[out.protocol] {
		return out, fmt.Errorf("不支持的协议：%s", protocol)
	}
	if out.protocol == "direct" {
		// A direct pseudo-node has no server endpoint.
		out.address = ""
		out.port = nil
	} else {
		if out.address == "" {
			return out, fmt.Errorf("服务器地址不能为空")
		}
		if out.port == nil || *out.port < 1 || *out.port > 65535 {
			return out, fmt.Errorf("端口必须在 1 到 65535 之间")
		}
	}
	cfg := strings.TrimSpace(configJSON)
	if cfg == "" {
		cfg = "{}"
	}
	// The config must be a JSON OBJECT: arrays, strings, numbers,
	// booleans and null are all rejected. (Decoding into a map
	// directly would silently accept "null", so decode into any
	// and assert the shape.)
	var decoded any
	if err := json.Unmarshal([]byte(cfg), &decoded); err != nil {
		return out, fmt.Errorf("高级配置必须是有效的 JSON 对象")
	}
	obj, ok := decoded.(map[string]any)
	if !ok {
		return out, fmt.Errorf("高级配置必须是有效的 JSON 对象")
	}
	// Credential fields are rejected ANYWHERE in the value tree:
	// a nested {"auth":{"password":…}} or an array element
	// {"servers":[{"uuid":…}]} would otherwise smuggle plaintext
	// credentials into SQLite, bypassing the vault.
	if key, found := findSecretConfigKey(obj); found {
		return out, fmt.Errorf("高级配置不能包含凭据字段 %q（包括嵌套对象与数组中的字段），请使用凭据输入框", key)
	}
	out.configJSON = cfg
	return out, nil
}

// findSecretConfigKey walks a decoded JSON value tree and returns
// the first object key — at any depth, inside nested objects and
// array elements alike — that names a credential field. Key
// comparison is case-insensitive, matching how the top-level check
// has always behaved.
func findSecretConfigKey(value any) (string, bool) {
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if secretConfigKeys[strings.ToLower(key)] {
				return key, true
			}
			if found, ok := findSecretConfigKey(child); ok {
				return found, true
			}
		}
	case []any:
		for _, child := range node {
			if found, ok := findSecretConfigKey(child); ok {
				return found, true
			}
		}
	}
	return "", false
}

// validateSecrets bounds a secrets map before it reaches the
// vault: field names and values must both be non-empty.
func validateSecrets(secrets map[string]string) error {
	for key, value := range secrets {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("凭据字段名不能为空")
		}
		if value == "" {
			return fmt.Errorf("凭据字段 %q 的值不能为空", key)
		}
	}
	return nil
}

// mapProxyConstraintError turns SQLite constraint failures on the
// proxy tables into messages the UI can show directly.
func mapProxyConstraintError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: index 'idx_proxies_dedup'"),
		strings.Contains(msg, "UNIQUE constraint failed: proxies.dedup_key"):
		return fmt.Errorf("存在具有相同去重指纹的节点")
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("关联数据不存在或已被删除")
	default:
		return err
	}
}
