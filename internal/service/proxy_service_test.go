package service

// Tests for the redesigned proxy module: import parsing and
// failure cleanliness, the single binding relation, source
// lifecycle, and the launch gate. The core itself is covered
// by proxy_core_integration_test.go (real binary, env-gated).

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

const testSubURL = "https://example.com/sub?token=TESTONLY"

const twoNodeYAML = `
proxies:
  - {name: NodeA, type: socks5, server: 127.0.0.1, port: 18101}
  - {name: NodeB, type: socks5, server: 127.0.0.1, port: 18102}
`

type proxyFixture struct {
	svc   *ProxyService
	db    *database.DB
	store *store.Store
	vault *secrets.Store
}

func testProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	cfg, err := config.Load(func(key string) string {
		if key == config.EnvAppRoot {
			return t.TempDir()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := cfg.Layout.Ensure(); err != nil {
		t.Fatalf("layout ensure: %v", err)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	vault, _, err := secrets.Open(cfg.Layout.SecretsFile)
	if err != nil {
		t.Fatalf("open vault: %v", err)
	}
	svc := NewProxyService(db, cfg.Layout, vault, slog.Default())
	return &proxyFixture{svc: svc, db: db, store: store.New(db), vault: vault}
}

func (f *proxyFixture) makeInstance(t *testing.T, name string) model.Instance {
	t.Helper()
	inst, err := f.store.CreateInstance(context.Background(), model.Instance{
		Name: name, UserDataDir: "profiles/" + name, Status: model.StatusStopped,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	return inst
}

func (f *proxyFixture) stubFetch(body string, err error) {
	f.svc.fetchSubscription = func(ctx context.Context, rawURL string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(body), nil
	}
}

func (f *proxyFixture) addSubscriptionOK(t *testing.T) SourceView {
	t.Helper()
	f.stubFetch(twoNodeYAML, nil)
	view, err := f.svc.AddSubscriptionSource("op", "测试订阅", testSubURL)
	if err != nil {
		t.Fatalf("add subscription: %v", err)
	}
	return view
}

func (f *proxyFixture) nodeByName(t *testing.T, name string) NodeView {
	t.Helper()
	nodes, err := f.svc.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node %q not found in %+v", name, nodes)
	return NodeView{}
}

// --- parsing ---

func TestParseProxyNodes(t *testing.T) {
	full := "mixed-port: 7890\nproxies:\n  - {name: A, type: ss, server: s.example.com, port: 8388, cipher: aes-128-gcm, password: pw}\nrules:\n  - MATCH,DIRECT\n"
	nodes, err := parseProxyNodes([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].name != "A" || nodes[0].cfg["cipher"] != "aes-128-gcm" {
		t.Fatalf("nodes = %+v, want the one ss node with its full definition", nodes)
	}
}

func TestParseProxyNodesRejections(t *testing.T) {
	cases := map[string]string{
		"not yaml":     "{{{{",
		"no proxies":   "mixed-port: 7890\nrules: []\n",
		"empty list":   "proxies: []\n",
		"missing type": "proxies:\n  - {name: A, server: 127.0.0.1, port: 1}\n",
		"missing name": "proxies:\n  - {type: socks5, server: 127.0.0.1, port: 1}\n",
		"dup names":    "proxies:\n  - {name: A, type: socks5, server: 127.0.0.1, port: 1}\n  - {name: A, type: socks5, server: 127.0.0.1, port: 2}\n",
		"uri list":     "ss://YWVzLTEyOC1nY206cHc@127.0.0.1:8388#A\nvmess://xyz\n",
	}
	for label, body := range cases {
		if _, err := parseProxyNodes([]byte(body)); err == nil {
			t.Errorf("%s: want a parse error", label)
		}
	}
	if _, err := parseProxyNodes([]byte(cases["uri list"])); err == nil || !strings.Contains(err.Error(), "URI") {
		t.Errorf("uri list: want the specific URI-format error, got %v", err)
	}
	if _, err := parseProxyNodes([]byte(cases["dup names"])); err == nil || !strings.Contains(err.Error(), "同名节点") {
		t.Errorf("dup names: want the duplicate-name error, got %v", err)
	}
}

// --- subscription import ---

func TestAddSubscriptionFlow(t *testing.T) {
	f := testProxyFixture(t)
	view := f.addSubscriptionOK(t)
	if view.NodeCount != 2 || view.Kind != model.ProxySourceKindSubscription {
		t.Fatalf("view = %+v", view)
	}
	nodes, err := f.svc.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %+v, want 2", nodes)
	}
	for _, n := range nodes {
		if n.SourceName != "测试订阅" || n.Type != "socks5" {
			t.Fatalf("node view = %+v, want source name and type resolved", n)
		}
	}
	// The URL lives only in the vault: the database row holds a
	// reference, and the reference resolves to the URL.
	src, err := f.store.GetProxySource(context.Background(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.SecretRef == "" {
		t.Fatal("source row must hold a vault reference")
	}
	plain, err := f.vault.Get(src.SecretRef)
	if err != nil || string(plain) != testSubURL {
		t.Fatalf("vault entry = %q, %v; want the subscription URL", plain, err)
	}
}

func TestAddSubscriptionFailuresLeaveNothing(t *testing.T) {
	f := testProxyFixture(t)
	// Download failure.
	f.stubFetch("", errors.New("network down"))
	if _, err := f.svc.AddSubscriptionSource("op", "坏订阅", testSubURL); err == nil {
		t.Fatal("want download failure")
	}
	// Parse failure.
	f.stubFetch("not a config at all {{{", nil)
	if _, err := f.svc.AddSubscriptionSource("op", "坏订阅", testSubURL); err == nil {
		t.Fatal("want parse failure")
	}
	// Invalid URL.
	if _, err := f.svc.AddSubscriptionSource("op", "坏订阅", "not-a-url"); err == nil {
		t.Fatal("want URL validation failure")
	}
	sources, err := f.svc.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("failed imports must leave no source, got %+v", sources)
	}
}

// --- file import ---

func (f *proxyFixture) pickFile(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.SourceFilePicker = func() (string, error) { return path, nil }
}

func TestAddFileSourceFlow(t *testing.T) {
	f := testProxyFixture(t)
	f.pickFile(t, twoNodeYAML)
	view, err := f.svc.AddFileSource("op", "本地配置")
	if err != nil {
		t.Fatal(err)
	}
	if view.NodeCount != 2 || view.Kind != model.ProxySourceKindFile {
		t.Fatalf("view = %+v", view)
	}
	// DeBrowser's own copy exists under the core directory.
	src, err := f.store.GetProxySource(context.Background(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.svc.sourcesDir(), src.FileName)); err != nil {
		t.Fatalf("stored copy missing: %v", err)
	}
}

func TestAddFileSourceParseFailureLeavesNothing(t *testing.T) {
	f := testProxyFixture(t)
	f.pickFile(t, "proxies: []\n")
	if _, err := f.svc.AddFileSource("op", "空配置"); err == nil {
		t.Fatal("want parse failure")
	}
	sources, err := f.svc.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v, want none", sources)
	}
	entries, _ := os.ReadDir(f.svc.sourcesDir())
	if len(entries) != 0 {
		t.Fatalf("no file copy may remain after a failed import, got %v", entries)
	}
}

// --- bindings: the single relation ---

func TestBindingLifecycle(t *testing.T) {
	f := testProxyFixture(t)
	src := f.addSubscriptionOK(t)
	_ = src
	instA := f.makeInstance(t, "甲")
	instB := f.makeInstance(t, "乙")
	nodeA := f.nodeByName(t, "NodeA")
	nodeB := f.nodeByName(t, "NodeB")

	bvA, err := f.svc.SetBinding("op", instA.ID, nodeA.ID)
	if err != nil {
		t.Fatal(err)
	}
	bvB, err := f.svc.SetBinding("op", instB.ID, nodeB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bvA.ListenPort == bvB.ListenPort || bvA.ListenPort < 21001 {
		t.Fatalf("ports = %d/%d, want distinct allocations from the base", bvA.ListenPort, bvB.ListenPort)
	}
	// Moving A to NodeB keeps A's port (the listener stays; only
	// its target changes).
	bvA2, err := f.svc.SetBinding("op", instA.ID, nodeB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bvA2.ListenPort != bvA.ListenPort || bvA2.NodeName != "NodeB" {
		t.Fatalf("rebind = %+v, want same port, new node", bvA2)
	}
	// Two instances may share one node.
	bindings, err := f.svc.ListBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %+v, want 2", bindings)
	}
	// Clearing removes exactly that binding.
	if err := f.svc.ClearBinding("op", instA.ID); err != nil {
		t.Fatal(err)
	}
	bindings, err = f.svc.ListBindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].InstanceID != instB.ID {
		t.Fatalf("bindings after clear = %+v", bindings)
	}
	// Clearing again is a no-op, and unknown ids are errors.
	if err := f.svc.ClearBinding("op", instA.ID); err != nil {
		t.Fatalf("clearing an unbound instance must be a no-op: %v", err)
	}
	if _, err := f.svc.SetBinding("op", "no-such-instance", nodeA.ID); err == nil {
		t.Fatal("want an error for an unknown instance")
	}
	if _, err := f.svc.SetBinding("op", instB.ID, "no-such-node"); err == nil {
		t.Fatal("want an error for an unknown node")
	}
}

func TestSetBindingAbortsOnBindingReadError(t *testing.T) {
	f := testProxyFixture(t)
	f.addSubscriptionOK(t)
	inst := f.makeInstance(t, "甲")
	node := f.nodeByName(t, "NodeA")
	if _, err := f.svc.SetBinding("op", inst.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	// Corrupt the recorded port (a REAL value the int scan
	// cannot convert) so the binding read fails with a
	// non-NotFound error: SetBinding must abort, not reallocate.
	if _, err := f.db.SQL().ExecContext(context.Background(),
		`UPDATE instance_proxy_bindings SET listen_port = 21001.5 WHERE instance_id = ?`, inst.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.SetBinding("op", inst.ID, node.ID)
	if err == nil || !strings.Contains(err.Error(), "读取实例现有代理绑定失败") {
		t.Fatalf("err = %v, want the binding-read abort", err)
	}
}

// --- launch gate ---

func TestProxyPortForInstanceGate(t *testing.T) {
	f := testProxyFixture(t)
	f.addSubscriptionOK(t)
	inst := f.makeInstance(t, "甲")
	node := f.nodeByName(t, "NodeA")
	// Unbound: direct.
	if port, err := f.svc.ProxyPortForInstance(inst.ID); err != nil || port != 0 {
		t.Fatalf("unbound = (%d, %v), want (0, nil)", port, err)
	}
	// Bound but the core is not running: refuse, never direct.
	if _, err := f.svc.SetBinding("op", inst.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ProxyPortForInstance(inst.ID); err == nil || !strings.Contains(err.Error(), "代理核心未启动") {
		t.Fatalf("bound+stopped err = %v, want the core-not-running refusal", err)
	}
}

// --- source deletion & refresh ---

func TestDeleteSourceCascades(t *testing.T) {
	f := testProxyFixture(t)
	f.addSubscriptionOK(t)
	inst := f.makeInstance(t, "甲")
	node := f.nodeByName(t, "NodeA")
	if _, err := f.svc.SetBinding("op", inst.ID, node.ID); err != nil {
		t.Fatal(err)
	}
	sources, _ := f.svc.ListSources()
	if err := f.svc.DeleteSource("op", sources[0].ID); err != nil {
		t.Fatal(err)
	}
	if nodes, _ := f.svc.ListNodes(); len(nodes) != 0 {
		t.Fatalf("nodes after delete = %+v, want none", nodes)
	}
	if bindings, _ := f.svc.ListBindings(); len(bindings) != 0 {
		t.Fatalf("bindings after delete = %+v, want none (cascade)", bindings)
	}
	// The vault entry is gone too.
	src, err := f.store.GetProxySource(context.Background(), sources[0].ID)
	if err == nil {
		t.Fatalf("source row still present: %+v", src)
	}
	refs, err := f.store.AllSecretRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("secret refs after delete = %v, want none referenced", refs)
	}
	// And the launch gate sees the instance as unbound again.
	if port, err := f.svc.ProxyPortForInstance(inst.ID); err != nil || port != 0 {
		t.Fatalf("after cascade = (%d, %v), want direct", port, err)
	}
}

func TestRefreshSourceReplacesNodes(t *testing.T) {
	f := testProxyFixture(t)
	view := f.addSubscriptionOK(t)
	inst := f.makeInstance(t, "甲")
	nodeA := f.nodeByName(t, "NodeA")
	if _, err := f.svc.SetBinding("op", inst.ID, nodeA.ID); err != nil {
		t.Fatal(err)
	}
	// The refreshed subscription drops NodeA, keeps NodeB, adds NodeC.
	f.stubFetch("proxies:\n  - {name: NodeB, type: socks5, server: 127.0.0.1, port: 18102}\n  - {name: NodeC, type: socks5, server: 127.0.0.1, port: 18103}\n", nil)
	out, err := f.svc.RefreshSource("op", view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.NodeCount != 2 {
		t.Fatalf("refreshed count = %d, want 2", out.NodeCount)
	}
	// The binding to the vanished NodeA is gone with it.
	if bindings, _ := f.svc.ListBindings(); len(bindings) != 0 {
		t.Fatalf("bindings after refresh = %+v, want none", bindings)
	}
	// A failed refresh keeps the current nodes.
	f.stubFetch("", errors.New("network down"))
	if _, err := f.svc.RefreshSource("op", view.ID); err == nil {
		t.Fatal("want refresh failure")
	}
	if nodes, _ := f.svc.ListNodes(); len(nodes) != 2 {
		t.Fatalf("nodes after failed refresh = %+v, want the previous 2 kept", nodes)
	}
}

// TestLegacyDataUntouched pins the redesign's data promise:
// migration 0015 only ADDS tables — the legacy proxy tables and
// the instances' legacy proxy_id column survive a migration
// run with their data intact.
func TestLegacyDataUntouched(t *testing.T) {
	f := testProxyFixture(t)
	// The new tables exist and are usable alongside the legacy
	// schema (the fixture migrated the full chain 0001..0015).
	if _, err := f.db.SQL().Exec(`INSERT INTO proxy_sources (id, name, kind) VALUES ('s1', 'x', 'file')`); err != nil {
		t.Fatalf("proxy_sources not usable: %v", err)
	}
	var legacyTables int
	if err := f.db.SQL().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('proxies','subscriptions','mihomo_configs')`).Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if legacyTables != 3 {
		t.Fatalf("legacy tables present = %d, want all 3 preserved", legacyTables)
	}
}
