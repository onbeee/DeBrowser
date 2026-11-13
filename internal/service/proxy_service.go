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
	GroupID          *string  `json:"groupId,omitempty"`
	GroupName        string   `json:"groupName"`
	SortOrder        int      `json:"sortOrder"`
	Source           string   `json:"source"`
	SubscriptionID   *string  `json:"subscriptionId,omitempty"`
	Tags             []string `json:"tags"`
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
	GroupID    *string           `json:"groupId"`
	SortOrder  int               `json:"sortOrder"`
	Tags       []string          `json:"tags"`
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
	GroupID      *string           `json:"groupId"`
	SortOrder    int               `json:"sortOrder"`
	Enabled      bool              `json:"enabled"`
	Tags         []string          `json:"tags"`
	Secrets      map[string]string `json:"secrets"`
	ClearSecrets bool              `json:"clearSecrets"`
}

// ProxyGroupView is a group plus its proxy count.
type ProxyGroupView struct {
	model.ProxyGroup
	ProxyCount int `json:"proxyCount"`
}

// CreateGroupInput carries the editable fields of a group.
type CreateGroupInput struct {
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
}

// UpdateGroupInput carries the editable fields of one group.
type UpdateGroupInput struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
}

// TestProxyResult reports one connection test. The test is a TCP
// reachability check against the node's server endpoint (the only
// honest test available before the Mihomo core lands in Stage 4C;
// protocol-level delay and exit-IP tests arrive there).
type TestProxyResult struct {
	Proxy     ProxyView `json:"proxy"`
	OK        bool      `json:"ok"`
	LatencyMs *int      `json:"latencyMs,omitempty"`
	Message   string    `json:"message"`
}

// ProxyPortRange is the instance listener port range (Stage 4C
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
	ids := make([]string, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	tags, err := s.store.TagsForProxies(c, ids)
	if err != nil {
		return nil, err
	}
	groups, err := s.groupNames(c)
	if err != nil {
		return nil, err
	}
	out := make([]ProxyView, 0, len(list))
	for _, p := range list {
		out = append(out, s.toView(p, tags[p.ID], groups))
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
	tags, err := s.store.TagsForProxy(c, id)
	if err != nil {
		return ProxyView{}, err
	}
	groups, err := s.groupNames(c)
	if err != nil {
		return ProxyView{}, err
	}
	return s.toView(p, tags, groups), nil
}

func (s *ProxyService) groupNames(c context.Context) (map[string]string, error) {
	groups, err := s.store.ListProxyGroups(c)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(groups))
	for _, g := range groups {
		out[g.ID] = g.Name
	}
	return out, nil
}

// toView projects a stored proxy into its credential-free view,
// resolving the credential status against the vault. A reference
// that cannot be loaded yields CredentialUnavailable — never a
// silent downgrade to "no credential".
func (s *ProxyService) toView(p model.Proxy, tags []string, groupNames map[string]string) ProxyView {
	v := ProxyView{
		ID: p.ID, Name: p.Name, Protocol: p.Protocol, Address: p.Address,
		Port: p.Port, ConfigJSON: p.ConfigJSON, Enabled: p.Enabled,
		GroupID: p.GroupID, SortOrder: p.SortOrder, Source: p.Source,
		SubscriptionID: p.SubscriptionID, Tags: tags,
		LastLatencyMs: p.LastLatencyMs, LastTestOK: p.LastTestOK,
		LastTestedAt: p.LastTestedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		SecretFields: []string{},
	}
	if v.Tags == nil {
		v.Tags = []string{}
	}
	if p.GroupID != nil {
		v.GroupName = groupNames[*p.GroupID]
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
	tags, err := normalizeTags(input.Tags)
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
	if input.GroupID != nil {
		if _, err := s.store.GetProxyGroup(c, *input.GroupID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ProxyView{}, fmt.Errorf("所选分组不存在")
			}
			return ProxyView{}, err
		}
	}

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
		Enabled: true, GroupID: input.GroupID, SortOrder: input.SortOrder,
		Source: "manual",
	}
	var created model.Proxy
	txErr := s.withTx(c, func(tx *store.Store) error {
		var err error
		created, err = tx.CreateProxy(c, row)
		if err != nil {
			return err
		}
		return tx.SetProxyTags(c, created.ID, tags)
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
	groups, err := s.groupNames(c)
	if err != nil {
		return ProxyView{}, err
	}
	return s.toView(created, tags, groups), nil
}

// UpdateProxy validates and updates a proxy, applying the secret
// semantics documented on UpdateProxyInput.
func (s *ProxyService) UpdateProxy(input UpdateProxyInput) (ProxyView, error) {
	fields, err := validateProxyFields(input.Name, input.Protocol, input.Address, input.Port, input.ConfigJSON)
	if err != nil {
		return ProxyView{}, err
	}
	tags, err := normalizeTags(input.Tags)
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
	if input.GroupID != nil {
		if _, err := s.store.GetProxyGroup(c, *input.GroupID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ProxyView{}, fmt.Errorf("所选分组不存在")
			}
			return ProxyView{}, err
		}
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
	row.GroupID = input.GroupID
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
		if err != nil {
			return err
		}
		return tx.SetProxyTags(c, row.ID, tags)
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
	groups, err := s.groupNames(c)
	if err != nil {
		return ProxyView{}, err
	}
	return s.toView(updated, tags, groups), nil
}

// DeleteProxy removes a proxy: the database row (and, via schema
// cascades, its tag rows; referencing instances get proxy_id NULL)
// goes first, then the vault entry is cleaned under the
// live-reference guard.
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
// Groups and tags
// ----------------------------------------------------------------

// ListGroups returns all groups with their proxy counts.
func (s *ProxyService) ListGroups() ([]ProxyGroupView, error) {
	c, cancel := ctx()
	defer cancel()
	groups, err := s.store.ListProxyGroups(c)
	if err != nil {
		return nil, err
	}
	counts, err := s.store.ProxyGroupCounts(c)
	if err != nil {
		return nil, err
	}
	out := make([]ProxyGroupView, 0, len(groups))
	for _, g := range groups {
		out = append(out, ProxyGroupView{ProxyGroup: g, ProxyCount: counts[g.ID]})
	}
	return out, nil
}

// CreateGroup creates a proxy group.
func (s *ProxyService) CreateGroup(input CreateGroupInput) (ProxyGroupView, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return ProxyGroupView{}, fmt.Errorf("分组名称不能为空")
	}
	c, cancel := ctx()
	defer cancel()
	g, err := s.store.CreateProxyGroup(c, model.ProxyGroup{Name: name, SortOrder: input.SortOrder})
	if err != nil {
		return ProxyGroupView{}, mapProxyConstraintError(err)
	}
	return ProxyGroupView{ProxyGroup: g, ProxyCount: 0}, nil
}

// UpdateGroup renames or reorders a group.
func (s *ProxyService) UpdateGroup(input UpdateGroupInput) (ProxyGroupView, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return ProxyGroupView{}, fmt.Errorf("分组名称不能为空")
	}
	c, cancel := ctx()
	defer cancel()
	g, err := s.store.UpdateProxyGroup(c, model.ProxyGroup{ID: input.ID, Name: name, SortOrder: input.SortOrder})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ProxyGroupView{}, fmt.Errorf("分组不存在")
		}
		return ProxyGroupView{}, mapProxyConstraintError(err)
	}
	counts, err := s.store.ProxyGroupCounts(c)
	if err != nil {
		return ProxyGroupView{}, err
	}
	return ProxyGroupView{ProxyGroup: g, ProxyCount: counts[g.ID]}, nil
}

// DeleteGroup removes a group; its proxies become ungrouped
// (schema ON DELETE SET NULL).
func (s *ProxyService) DeleteGroup(id string) error {
	c, cancel := ctx()
	defer cancel()
	if err := s.store.DeleteProxyGroup(c, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("分组不存在")
		}
		return err
	}
	return nil
}

// ListTags returns every tag in use with its proxy count.
func (s *ProxyService) ListTags() ([]store.TagCount, error) {
	c, cancel := ctx()
	defer cancel()
	return s.store.ListTagCounts(c)
}

// DeleteTag removes a tag label from every proxy carrying it and
// returns how many associations were removed.
func (s *ProxyService) DeleteTag(tag string) (int, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return 0, fmt.Errorf("标签不能为空")
	}
	c, cancel := ctx()
	defer cancel()
	n, err := s.store.DeleteTagEverywhere(c, tag)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// RenameTag renames a tag label on every proxy carrying it.
func (s *ProxyService) RenameTag(oldTag, newTag string) error {
	oldTag = strings.TrimSpace(oldTag)
	newTag = strings.TrimSpace(newTag)
	if oldTag == "" || newTag == "" {
		return fmt.Errorf("标签不能为空")
	}
	if oldTag == newTag {
		return fmt.Errorf("新标签与原标签相同")
	}
	c, cancel := ctx()
	defer cancel()
	return s.withTx(c, func(tx *store.Store) error {
		return tx.RenameTagEverywhere(c, oldTag, newTag)
	})
}

// ----------------------------------------------------------------
// Port range (Stage 4C groundwork)
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
	var obj map[string]any
	if err := json.Unmarshal([]byte(cfg), &obj); err != nil {
		return out, fmt.Errorf("高级配置必须是有效的 JSON 对象")
	}
	for key := range obj {
		if secretConfigKeys[strings.ToLower(key)] {
			return out, fmt.Errorf("高级配置不能包含凭据字段 %q，请使用凭据输入框", key)
		}
	}
	out.configJSON = cfg
	return out, nil
}

// normalizeTags trims, deduplicates and bounds a tag list.
func normalizeTags(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		if len([]rune(tag)) > 64 {
			return nil, fmt.Errorf("标签 %q 过长（最多 64 个字符）", tag)
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > 20 {
		return nil, fmt.Errorf("标签数量不能超过 20 个")
	}
	sort.Strings(out)
	return out, nil
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
	case strings.Contains(msg, "UNIQUE constraint failed: proxy_groups.name"):
		return fmt.Errorf("分组名称已存在，请换一个名称")
	case strings.Contains(msg, "UNIQUE constraint failed: index 'idx_proxies_dedup'"),
		strings.Contains(msg, "UNIQUE constraint failed: proxies.dedup_key"):
		return fmt.Errorf("存在具有相同去重指纹的节点")
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("关联数据不存在或已被删除")
	default:
		return err
	}
}
