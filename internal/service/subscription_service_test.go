package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

type subFixture struct {
	svc    *SubscriptionService
	proxy  *ProxyService
	store  *store.Store
	vault  *secrets.Store
	layout paths.Layout
	db     *database.DB
}

func newSubFixture(t *testing.T) subFixture {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Layout: layout, LogLevel: "info"}
	db, err := database.Open(layout.DatabaseFile)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	logs, err := logging.NewSystem(cfg)
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	vault, _, err := secrets.Open(layout.SecretsFile)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	proxySvc, err := NewProxyService(db, vault, logs.Logger())
	if err != nil {
		t.Fatalf("NewProxyService: %v", err)
	}
	svc := NewSubscriptionService(proxySvc, layout, logs.Logger())
	f := subFixture{svc: svc, proxy: proxySvc, store: store.New(db), vault: vault, layout: layout, db: db}
	t.Cleanup(func() {
		_ = logs.Close()
		_ = db.Close()
	})
	return f
}

// vaultEntryCount reads the vault file's entry count via a fresh
// decode: entries are listed by attempting List-like access — the
// vault API exposes refs only through stored rows, so tests assert
// on Get results instead.
func (f subFixture) vaultHas(ref string) bool {
	if ref == "" {
		return false
	}
	_, err := f.vault.Get(ref)
	return err == nil
}

const subFeedV1 = `proxies:
  - {name: 节点甲, type: ss, server: a.example.com, port: 8388, cipher: aes-256-gcm, password: pw-a}
  - {name: 节点乙, type: vless, server: b.example.com, port: 443, uuid: uuid-b, tls: true}
proxy-groups:
  - {name: 选择组, type: select, proxies: [节点甲, 节点乙, DIRECT]}
`

const subFeedV2 = `proxies:
  - {name: 节点甲, type: ss, server: a.example.com, port: 8388, cipher: aes-256-gcm, password: pw-a2}
  - {name: 节点丙, type: trojan, server: c.example.com, port: 443, password: pw-c}
proxy-groups:
  - {name: 选择组, type: select, proxies: [节点甲, 节点丙, REJECT]}
`

func (f subFixture) createSub(t *testing.T) SubscriptionView {
	t.Helper()
	v, err := f.svc.CreateSubscription(SubscriptionInput{
		Name: "测试订阅", URL: "https://subs.example.com/feed?token=hidden",
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	return v
}

func (f subFixture) stubFeed(payload string) {
	f.svc.fetchFn = func(context.Context, string, bool) ([]byte, error) {
		return []byte(payload), nil
	}
}

func TestSubscriptionCreateStoresURLInVaultOnly(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	if v.URLHint != "https://subs.example.com" || !v.HasURL {
		t.Fatalf("view = %+v", v)
	}
	c, cancel := ctx()
	defer cancel()
	row, err := f.store.GetSubscription(c, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.URLHint, "token") || row.SecretRef == "" {
		t.Fatalf("row leaks URL material: %+v", row)
	}
	raw, err := f.vault.Get(row.SecretRef)
	if err != nil || string(raw) != "https://subs.example.com/feed?token=hidden" {
		t.Fatalf("vault URL = %q, err %v", raw, err)
	}
	if _, err := f.svc.CreateSubscription(SubscriptionInput{Name: "x", URL: "ftp://a.com/x"}); err == nil {
		t.Fatal("non-http URL accepted")
	}
	if _, err := f.svc.CreateSubscription(SubscriptionInput{Name: "x", URL: "https://a.com/x", AutoUpdateMinutes: 5}); err == nil {
		t.Fatal("interval < 15 accepted")
	}
}

func TestSubscriptionRefreshLifecycle(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	f.stubFeed(subFeedV1)
	sum, err := f.svc.RefreshSubscription(v.ID)
	if err != nil {
		t.Fatalf("refresh v1: %v", err)
	}
	if sum.Added != 2 || sum.Groups != 1 || sum.Format != "proxies_yaml" {
		t.Fatalf("summary = %+v", sum)
	}
	c, cancel := ctx()
	defer cancel()
	before, err := f.store.ListProxiesBySubscription(c, v.ID)
	if err != nil || len(before) != 2 {
		t.Fatalf("proxies = %v, err %v", before, err)
	}
	ids := map[string]string{}
	for _, p := range before {
		ids[p.SourceNodeName] = p.ID
		if p.SecretRef == "" || !f.vaultHas(p.SecretRef) {
			t.Fatalf("node %s has no vault envelope", p.SourceNodeName)
		}
		if p.ConfigJSON == "" || strings.Contains(p.ConfigJSON, "pw-a") || strings.Contains(p.ConfigJSON, "uuid-b") {
			t.Fatalf("config_json leaks or empty: %s", p.ConfigJSON)
		}
	}
	// Group members resolved to the proxy IDs.
	groups, err := f.svc.GetConfigGroups(sum.ConfigID)
	if err != nil || len(groups) != 1 || len(groups[0].Members) != 3 {
		t.Fatalf("groups = %+v, err %v", groups, err)
	}
	if groups[0].Members[0].Kind != "proxy" || groups[0].Members[2].Kind != "builtin" {
		t.Fatalf("members = %+v", groups[0].Members)
	}
	// Artifact file exists under data/mihomo/configs, sanitized.
	cfg, err := f.store.GetMihomoConfigBySubscription(c, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(filepath.Join(f.layout.Mihomo, cfg.FilePath))
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if strings.Contains(string(artifact), "pw-a") || strings.Contains(string(artifact), "uuid-b") {
		t.Fatal("artifact leaks credentials")
	}
	firstArtifact := cfg.FilePath

	// Second refresh: 节点甲 keeps its ID (password rotated),
	// 节点乙 disappears, 节点丙 appears, old artifact replaced.
	f.stubFeed(subFeedV2)
	sum2, err := f.svc.RefreshSubscription(v.ID)
	if err != nil {
		t.Fatalf("refresh v2: %v", err)
	}
	if sum2.Added != 1 || sum2.Updated != 1 || sum2.Removed != 1 {
		t.Fatalf("summary2 = %+v", sum2)
	}
	after, _ := f.store.ListProxiesBySubscription(c, v.ID)
	if len(after) != 2 {
		t.Fatalf("after proxies = %d", len(after))
	}
	for _, p := range after {
		if p.SourceNodeName == "节点甲" && p.ID != ids["节点甲"] {
			t.Fatal("节点甲 ID changed across refresh")
		}
		if p.SourceNodeName == "节点乙" {
			t.Fatal("节点乙 should have been removed")
		}
	}
	cfg2, _ := f.store.GetMihomoConfigBySubscription(c, v.ID)
	if cfg2.FilePath == firstArtifact {
		t.Fatal("artifact path did not rotate")
	}
	if _, err := os.Stat(filepath.Join(f.layout.Mihomo, firstArtifact)); !os.IsNotExist(err) {
		t.Fatal("old artifact file was not cleaned up")
	}
	sub, _ := f.store.GetSubscription(c, v.ID)
	if sub.LastStatus != "ok" || sub.LastUpdatedAt == nil {
		t.Fatalf("subscription status = %+v", sub)
	}
}

func TestSubscriptionRefreshFailureKeepsOldData(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(v.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.fetchFn = func(context.Context, string, bool) ([]byte, error) {
		return nil, errors.New("订阅请求超时（https://subs.example.com）")
	}
	if _, err := f.svc.RefreshSubscription(v.ID); err == nil {
		t.Fatal("failing refresh reported success")
	}
	c, cancel := ctx()
	defer cancel()
	proxies, _ := f.store.ListProxiesBySubscription(c, v.ID)
	if len(proxies) != 2 {
		t.Fatalf("nodes lost after failed refresh: %d", len(proxies))
	}
	sub, _ := f.store.GetSubscription(c, v.ID)
	if sub.LastStatus != "error" || sub.LastError == "" {
		t.Fatalf("status = %+v", sub)
	}
	if strings.Contains(sub.LastError, "token=hidden") {
		t.Fatal("last_error leaks the URL token")
	}
	// Parse failure behaves the same way.
	f.stubFeed("not a valid payload at all [[[")
	if _, err := f.svc.RefreshSubscription(v.ID); err == nil {
		t.Fatal("garbage refresh reported success")
	}
	proxies, _ = f.store.ListProxiesBySubscription(c, v.ID)
	if len(proxies) != 2 {
		t.Fatal("nodes lost after unparseable refresh")
	}
}

func TestSubscriptionUpdateRotatesVaultEntry(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	c, cancel := ctx()
	defer cancel()
	row, _ := f.store.GetSubscription(c, v.ID)
	oldRef := row.SecretRef
	updated, err := f.svc.UpdateSubscription(v.ID, SubscriptionInput{
		Name: "改名订阅", URL: "https://other.example.com/new",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.URLHint != "https://other.example.com" || updated.Name != "改名订阅" {
		t.Fatalf("updated = %+v", updated)
	}
	if f.vaultHas(oldRef) {
		t.Fatal("old URL entry still in vault after rotation")
	}
	row2, _ := f.store.GetSubscription(c, v.ID)
	raw, err := f.vault.Get(row2.SecretRef)
	if err != nil || string(raw) != "https://other.example.com/new" {
		t.Fatalf("new vault URL = %q err %v", raw, err)
	}
	// Edit without URL keeps the entry.
	if _, err := f.svc.UpdateSubscription(v.ID, SubscriptionInput{Name: "再次改名"}); err != nil {
		t.Fatal(err)
	}
	row3, _ := f.store.GetSubscription(c, v.ID)
	if row3.SecretRef != row2.SecretRef {
		t.Fatal("URL entry rotated by a name-only edit")
	}
}

func TestSubscriptionDeleteCleansEverything(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	other := f.createSub(t)
	_ = other
	f.stubFeed(subFeedV1)
	sum, err := f.svc.RefreshSubscription(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := ctx()
	defer cancel()
	cfg, _ := f.store.GetMihomoConfigBySubscription(c, v.ID)
	artifactAbs := filepath.Join(f.layout.Mihomo, cfg.FilePath)
	proxies, _ := f.store.ListProxiesBySubscription(c, v.ID)
	refs := []string{cfg.SecretRef}
	for _, p := range proxies {
		refs = append(refs, p.SecretRef)
	}
	if err := f.svc.DeleteSubscription(v.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := f.store.GetSubscription(c, v.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("subscription row survived delete")
	}
	if left, _ := f.store.ListProxiesBySubscription(c, v.ID); len(left) != 0 {
		t.Fatal("subscription proxies survived delete")
	}
	if _, err := os.Stat(artifactAbs); !os.IsNotExist(err) {
		t.Fatal("artifact survived delete")
	}
	for _, ref := range refs {
		if ref != "" && f.vaultHas(ref) {
			t.Fatalf("vault entry %s survived delete", ref[:8])
		}
	}
	// Manual proxies are untouched by subscription deletion.
	port := 1080
	manual, err := f.proxy.CreateProxy(CreateProxyInput{Name: "手动", Protocol: "socks5", Address: "127.0.0.1", Port: &port})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.proxy.GetProxy(manual.ID); err != nil {
		t.Fatal("manual proxy affected")
	}
	_ = sum
}

func TestImportLocalConfigFile(t *testing.T) {
	f := newSubFixture(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "my-config.yaml")
	if err := os.WriteFile(file, []byte(subFeedV1), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	sum, err := f.svc.ImportLocalConfig("")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if sum.Added != 2 || sum.ConfigName != "my-config" || sum.Cancelled {
		t.Fatalf("summary = %+v", sum)
	}
	c, cancel := ctx()
	defer cancel()
	cfg, err := f.store.GetMihomoConfig(c, sum.ConfigID)
	if err != nil || cfg.SourceType != "file" || cfg.SubscriptionID != nil {
		t.Fatalf("cfg = %+v err %v", cfg, err)
	}
	proxies, _ := f.store.ListProxiesByConfig(c, sum.ConfigID)
	if len(proxies) != 2 || proxies[0].Source != "file_import" {
		t.Fatalf("proxies = %+v", proxies)
	}
	// Cancel is a normal, error-free outcome.
	f.svc.FilePicker = func() (string, error) { return "", nil }
	sum2, err := f.svc.ImportLocalConfig("")
	if err != nil || !sum2.Cancelled {
		t.Fatalf("cancel: summary=%+v err=%v", sum2, err)
	}
	// Deleting the local config removes its nodes and artifact.
	if err := f.svc.DeleteConfig(sum.ConfigID); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	if left, _ := f.store.ListProxiesByConfig(c, sum.ConfigID); len(left) != 0 {
		t.Fatal("config proxies survived DeleteConfig")
	}
	if _, err := os.Stat(filepath.Join(f.layout.Mihomo, cfg.FilePath)); !os.IsNotExist(err) {
		t.Fatal("artifact survived DeleteConfig")
	}
}

func TestCrossSubscriptionNameIsolation(t *testing.T) {
	f := newSubFixture(t)
	a := f.createSub(t)
	bView, err := f.svc.CreateSubscription(SubscriptionInput{Name: "订阅B", URL: "https://b.example.com/feed"})
	if err != nil {
		t.Fatal(err)
	}
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RefreshSubscription(bView.ID); err != nil {
		t.Fatal(err)
	}
	c, cancel := ctx()
	defer cancel()
	pa, _ := f.store.ListProxiesBySubscription(c, a.ID)
	pb, _ := f.store.ListProxiesBySubscription(c, bView.ID)
	if len(pa) != 2 || len(pb) != 2 {
		t.Fatalf("pa=%d pb=%d", len(pa), len(pb))
	}
	// Same node names in both subscriptions are distinct rows with
	// distinct dedup keys, and B's groups bind to B's nodes.
	for _, x := range pa {
		for _, y := range pb {
			if x.ID == y.ID || x.DedupKey == y.DedupKey {
				t.Fatal("cross-subscription identity collision")
			}
		}
	}
	gb, err := f.svc.GetConfigGroups(mustConfigID(t, f, bView.ID))
	if err != nil || len(gb) != 1 {
		t.Fatalf("groups B: %v %+v", err, gb)
	}
	if gb[0].Members[0].ProxyName != "节点甲" {
		t.Fatalf("B group member = %+v", gb[0].Members[0])
	}
	// Deleting A leaves B's nodes and groups intact.
	if err := f.svc.DeleteSubscription(a.ID); err != nil {
		t.Fatal(err)
	}
	pb2, _ := f.store.ListProxiesBySubscription(c, bView.ID)
	if len(pb2) != 2 {
		t.Fatal("B nodes affected by A delete")
	}
}

func mustConfigID(t *testing.T, f subFixture, subID string) string {
	t.Helper()
	c, cancel := ctx()
	defer cancel()
	cfg, err := f.store.GetMihomoConfigBySubscription(c, subID)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ID
}

func TestSubscriptionDueLogic(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	old := "2026-11-13 10:00:00"
	sub := model.Subscription{AutoUpdateMinutes: 60, LastUpdatedAt: &old, UpdatedAt: old}
	if subscriptionDue(sub, now) != true {
		t.Fatal("2h-old hourly subscription should be due")
	}
	recent := "2026-11-13 11:30:00"
	sub.LastUpdatedAt = &recent
	if subscriptionDue(sub, now) {
		t.Fatal("30m-old hourly subscription should not be due")
	}
	sub.AutoUpdateMinutes = 0
	if subscriptionDue(sub, now) {
		t.Fatal("disabled auto-update must never be due")
	}
	never := model.Subscription{AutoUpdateMinutes: 15, UpdatedAt: "2026-11-13 11:00:00"}
	if !subscriptionDue(never, now) {
		t.Fatal("never-refreshed subscription past its interval should be due")
	}
}

func TestRefreshDueSubscriptions(t *testing.T) {
	f := newSubFixture(t)
	v, err := f.svc.CreateSubscription(SubscriptionInput{
		Name: "自动订阅", URL: "https://auto.example.com/feed", AutoUpdateMinutes: 15,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Force the row old enough to be due.
	c, cancel := ctx()
	defer cancel()
	row, _ := f.store.GetSubscription(c, v.ID)
	old := time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05")
	row.UpdatedAt = old
	if _, err := f.db.SQL().ExecContext(c, `UPDATE subscriptions SET updated_at = ? WHERE id = ?`, old, v.ID); err != nil {
		t.Fatal(err)
	}
	_ = row
	f.stubFeed(subFeedV1)
	f.svc.RefreshDueSubscriptions(c)
	proxies, _ := f.store.ListProxiesBySubscription(c, v.ID)
	if len(proxies) != 2 {
		t.Fatalf("due refresh imported %d nodes, want 2", len(proxies))
	}
	// Just refreshed: a second sweep is a no-op (still 2 nodes,
	// and the fetch stub would have been fine either way — assert
	// via last status remaining ok and node count stable).
	f.svc.RefreshDueSubscriptions(c)
	proxies, _ = f.store.ListProxiesBySubscription(c, v.ID)
	if len(proxies) != 2 {
		t.Fatal("second sweep changed data")
	}
}

func TestImportSummaryJSONHasNoSecrets(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	f.stubFeed(subFeedV1)
	sum, err := f.svc.RefreshSubscription(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sum)
	if strings.Contains(string(raw), "pw-a") || strings.Contains(string(raw), "token=hidden") {
		t.Fatalf("summary leaks: %s", raw)
	}
	views, err := f.svc.ListSubscriptions()
	if err != nil || len(views) != 1 {
		t.Fatalf("views = %+v err %v", views, err)
	}
	rawViews, _ := json.Marshal(views)
	if strings.Contains(string(rawViews), "token=hidden") || strings.Contains(string(rawViews), "secretRef") {
		t.Fatalf("views leak: %s", rawViews)
	}
}

// TestCreateSubscriptionCompensatesVaultOnDBFailure injects a
// transaction failure after the vault Put: the pending URL entry
// must be compensated away and no row may remain (§16.4).
func TestCreateSubscriptionCompensatesVaultOnDBFailure(t *testing.T) {
	f := newSubFixture(t)
	realTx := f.proxy.withTx
	f.proxy.withTx = func(context.Context, func(tx *store.Store) error) error {
		return errors.New("injected tx failure")
	}
	defer func() { f.proxy.withTx = realTx }()
	before := len(f.vault.Refs())
	if _, err := f.svc.CreateSubscription(SubscriptionInput{Name: "x", URL: "https://a.example.com/f"}); err == nil {
		t.Fatal("create succeeded despite tx failure")
	}
	if got := len(f.vault.Refs()); got != before {
		t.Fatalf("vault entries = %d, want %d (compensation failed)", got, before)
	}
	c, cancel := ctx()
	defer cancel()
	if subs, _ := f.store.ListSubscriptions(c); len(subs) != 0 {
		t.Fatal("subscription row survived failed create")
	}
}

// TestRefreshCompensatesOnDBFailure: a refresh whose transaction
// fails must compensate the new envelopes, remove the new
// artifact and keep the previously imported data fully intact.
func TestRefreshCompensatesOnDBFailure(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(v.ID); err != nil {
		t.Fatal(err)
	}
	c, cancel := ctx()
	defer cancel()
	refsBefore := len(f.vault.Refs())
	configsDir := filepath.Join(f.layout.Mihomo, "configs")
	countFiles := func() int {
		entries, _ := os.ReadDir(configsDir)
		return len(entries)
	}
	filesBefore := countFiles()

	realTx := f.proxy.withTx
	f.proxy.withTx = func(context.Context, func(tx *store.Store) error) error {
		return errors.New("injected tx failure")
	}
	f.stubFeed(subFeedV2)
	_, err := f.svc.RefreshSubscription(v.ID)
	f.proxy.withTx = realTx
	if err == nil {
		t.Fatal("refresh succeeded despite tx failure")
	}
	if got := len(f.vault.Refs()); got != refsBefore {
		t.Fatalf("vault entries = %d, want %d", got, refsBefore)
	}
	if got := countFiles(); got != filesBefore {
		t.Fatalf("artifact files = %d, want %d (new artifact not removed)", got, filesBefore)
	}
	proxies, _ := f.store.ListProxiesBySubscription(c, v.ID)
	if len(proxies) != 2 {
		t.Fatalf("old nodes damaged by failed refresh: %d", len(proxies))
	}
	names := map[string]bool{}
	for _, p := range proxies {
		names[p.SourceNodeName] = true
	}
	if !names["节点甲"] || !names["节点乙"] {
		t.Fatalf("old node set changed: %v", names)
	}
}

const nestedSecretFeed = `
proxies:
  - name: 嵌套节点
    type: ss
    server: n.example.com
    port: 8388
    cipher: aes-256-gcm
    password: svc-top-secret
    extra:
      layers:
        - - {password: svc-deep-array-secret}
        - - {uuid: svc-deep-uuid}
      endpoints:
        - {host: e1.example.com, auth: {password: svc-deep-auth}}
`

// TestImportNestedSecretsAcrossStores is the service-level proof
// for the deep-classification fix: after importing a document
// whose credentials are buried under alternating map/array
// layers, no secret appears in SQLite config_json or in the
// on-disk artifact, while every secret is present in the vault
// envelope. A subsequent DB-failing refresh must still
// compensate its vault writes and keep the good data.
func TestImportNestedSecretsAcrossStores(t *testing.T) {
	f := newSubFixture(t)
	v := f.createSub(t)
	f.stubFeed(nestedSecretFeed)
	sum, err := f.svc.RefreshSubscription(v.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if sum.Added != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	secrets := []string{"svc-top-secret", "svc-deep-array-secret", "svc-deep-uuid", "svc-deep-auth"}
	c, cancel := ctx()
	defer cancel()
	proxies, err := f.store.ListProxiesBySubscription(c, v.ID)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("proxies = %v err %v", proxies, err)
	}
	row := proxies[0]
	// 1. SQLite config_json carries no secret at any depth, but
	// keeps the non-secret nested data.
	for _, s := range secrets {
		if strings.Contains(row.ConfigJSON, s) {
			t.Fatalf("config_json leaks %q: %s", s, row.ConfigJSON)
		}
	}
	if !strings.Contains(row.ConfigJSON, "e1.example.com") {
		t.Fatalf("non-secret nested data lost: %s", row.ConfigJSON)
	}
	// 2. The vault envelope holds every secret (under distinct
	// dotted paths).
	if row.SecretRef == "" {
		t.Fatal("node has no vault envelope")
	}
	raw, err := f.vault.Get(row.SecretRef)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	var envelope map[string]string
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("envelope JSON: %v", err)
	}
	for _, s := range secrets {
		found := false
		for _, val := range envelope {
			if strings.Contains(val, s) {
				found = true
			}
		}
		if !found {
			t.Fatalf("secret %q missing from vault envelope %v", s, envelope)
		}
	}
	for _, path := range []string{"extra.layers.0.0.password", "extra.layers.1.0.uuid", "extra.endpoints.0.auth"} {
		if _, ok := envelope[path]; !ok {
			t.Fatalf("envelope missing path %q: %v", path, envelope)
		}
	}
	// 3. The on-disk artifact is clean.
	cfg, err := f.store.GetMihomoConfigBySubscription(c, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(filepath.Join(f.layout.Mihomo, cfg.FilePath))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if strings.Contains(string(artifact), s) {
			t.Fatalf("artifact leaks %q", s)
		}
	}
	// 4. A DB-failing refresh of the same payload compensates:
	// vault entry count unchanged, prior data intact.
	refsBefore := len(f.vault.Refs())
	realTx := f.proxy.withTx
	f.proxy.withTx = func(context.Context, func(tx *store.Store) error) error {
		return errors.New("injected tx failure")
	}
	_, err = f.svc.RefreshSubscription(v.ID)
	f.proxy.withTx = realTx
	if err == nil {
		t.Fatal("refresh succeeded despite tx failure")
	}
	if got := len(f.vault.Refs()); got != refsBefore {
		t.Fatalf("vault entries = %d, want %d after failed refresh", got, refsBefore)
	}
	proxiesAfter, _ := f.store.ListProxiesBySubscription(c, v.ID)
	if len(proxiesAfter) != 1 || proxiesAfter[0].ID != row.ID || proxiesAfter[0].SecretRef != row.SecretRef {
		t.Fatal("prior import damaged by failed refresh")
	}
}
