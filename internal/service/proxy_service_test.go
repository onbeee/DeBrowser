package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Stage 6C proxy service tests. The core itself is never started
// in this file (see proxy_core_integration_test.go for the
// real-core scenarios): every assertion runs against the
// database, the vault and the generated configuration file — the
// artifacts DeBrowser actually owns. Nothing here may start the
// core implicitly: the service has no autostart path at all.

type proxyFixture struct {
	svc   *ProxyService
	db    *database.DB
	vault *secrets.Store
	cfg   config.Config
	store *store.Store
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
		t.Fatal(err)
	}
	if err := cfg.Layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	vault, _, err := secrets.Open(cfg.Layout.SecretsFile)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewProxyService(db, cfg.Layout, vault, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return &proxyFixture{svc: svc, db: db, vault: vault, cfg: cfg, store: store.New(db)}
}

func (f *proxyFixture) makeInstance(t *testing.T, name string) model.Instance {
	t.Helper()
	id, err := store.NewID()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.store.CreateInstance(context.Background(), model.Instance{
		ID: id, Name: name, UserDataDir: "profiles/" + id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

const testSubURL = "https://sub.example.com/feed?token=do-not-log-me"

func TestAddURLSourceValidation(t *testing.T) {
	f := testProxyFixture(t)
	if _, err := f.svc.AddURLSource("op", "", testSubURL); err == nil {
		t.Error("empty name must fail")
	}
	if _, err := f.svc.AddURLSource("op", "机场", "ftp://example.com/x"); err == nil {
		t.Error("non-http(s) URL must fail")
	}
	if _, err := f.svc.AddURLSource("op", "机场", "not a url"); err == nil {
		t.Error("garbage URL must fail")
	}
}

func TestURLSourceVaultAndBindingFlow(t *testing.T) {
	f := testProxyFixture(t)
	ctx := context.Background()

	view, err := f.svc.AddURLSource("op", "机场一", testSubURL)
	if err != nil {
		t.Fatal(err)
	}
	if view.Kind != model.ProxySourceKindURL || view.ID == "" {
		t.Fatalf("unexpected view: %+v", view)
	}
	// The database holds only the vault reference; the URL itself
	// is in the vault and nowhere in the row's visible fields.
	src, err := f.store.GetProxySource(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if src.SecretRef == "" {
		t.Fatal("source row must carry a vault reference")
	}
	raw, err := f.vault.Get(src.SecretRef)
	if err != nil || string(raw) != testSubURL {
		t.Fatalf("vault roundtrip = %q, %v", raw, err)
	}

	inst1 := f.makeInstance(t, "实例一")
	inst2 := f.makeInstance(t, "实例二")

	b1, err := f.svc.SetBinding("op", inst1.ID, src.ID, "香港 01")
	if err != nil {
		t.Fatal(err)
	}
	if b1.Port != proxyListenerBasePort {
		t.Fatalf("first binding port = %d, want %d", b1.Port, proxyListenerBasePort)
	}
	b2, err := f.svc.SetBinding("op", inst2.ID, src.ID, "日本 01")
	if err != nil {
		t.Fatal(err)
	}
	if b2.Port != proxyListenerBasePort+1 {
		t.Fatalf("second binding port = %d, want %d", b2.Port, proxyListenerBasePort+1)
	}
	// Re-binding keeps the instance's port.
	b1again, err := f.svc.SetBinding("op", inst1.ID, src.ID, "香港 02")
	if err != nil {
		t.Fatal(err)
	}
	if b1again.Port != b1.Port || b1again.NodeName != "香港 02" {
		t.Fatalf("rebind = %+v", b1again)
	}
	// The core was never started (nothing starts it implicitly),
	// so a bound instance must NOT hand out its port: the launch
	// hook refuses with an error until the user starts the core
	// manually. An unbound instance is unaffected.
	if port, err := f.svc.ProxyPortForInstance(inst1.ID); err == nil || port != 0 {
		t.Fatalf("ProxyPortForInstance(bound, core stopped) = %d, %v; want 0 and an error", port, err)
	}
	if port, err := f.svc.ProxyPortForInstance("no-such-instance"); err != nil || port != 0 {
		t.Fatalf("ProxyPortForInstance(unbound) = %d, %v; want 0, nil", port, err)
	}

	// The generated configuration carries the whole wiring: the
	// subscription URL reaches only this file (inside the core
	// working directory); each instance has its own listener
	// handing traffic to its own REJECT-first group.
	if err := f.svc.regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	cfgBytes, err := os.ReadFile(filepath.Join(f.cfg.Layout.Mihomo, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfgText := string(cfgBytes)
	for _, want := range []string{
		"url: \"" + testSubURL + "\"",
		"port: 21001",
		"port: 21002",
		"proxy: \"inst-",
		"- REJECT",
		"proxy: DIRECT",
	} {
		if !strings.Contains(cfgText, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, cfgText)
		}
	}
	if strings.Contains(cfgText, "sub-rules:") {
		t.Errorf("generated config must not contain sub-rules\n%s", cfgText)
	}

	// A source with bindings cannot be deleted.
	if err := f.svc.DeleteSource("op", src.ID); err == nil {
		t.Fatal("deleting a bound source must fail")
	}
	// After clearing both bindings the delete succeeds and the
	// vault entry is collected.
	if err := f.svc.ClearBinding("op", inst1.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ClearBinding("op", inst2.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteSource("op", src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.vault.Get(src.SecretRef); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("vault entry after source delete: err = %v, want ErrNotFound", err)
	}
}

func TestSetBindingValidation(t *testing.T) {
	f := testProxyFixture(t)
	inst := f.makeInstance(t, "实例")
	if _, err := f.svc.SetBinding("op", inst.ID, "no-source", "N"); err == nil {
		t.Error("unknown source must fail")
	}
	view, err := f.svc.AddURLSource("op", "机场", testSubURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetBinding("op", "no-instance", view.ID, "N"); err == nil {
		t.Error("unknown instance must fail")
	}
	if _, err := f.svc.SetBinding("op", inst.ID, view.ID, ""); err == nil {
		t.Error("empty node must fail")
	}
	// An unbound instance reports bound=false, not an error.
	if _, bound, err := f.svc.GetBinding(inst.ID); err != nil || bound {
		t.Errorf("GetBinding unbound = bound %v err %v", bound, err)
	}
}

func TestAddFileSource(t *testing.T) {
	f := testProxyFixture(t)
	good := filepath.Join(t.TempDir(), "nodes.yaml")
	if err := os.WriteFile(good, []byte("proxies:\n  - {name: A, type: ss, server: 1.2.3.4, port: 8388, cipher: aes-256-gcm, password: x}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "notnodes.yaml")
	if err := os.WriteFile(bad, []byte("rules:\n  - MATCH,DIRECT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.SourceFilePicker = func() (string, error) { return bad, nil }
	if _, err := f.svc.AddFileSource("op", "坏文件"); err == nil {
		t.Fatal("a file without proxies must be rejected")
	}
	f.svc.SourceFilePicker = func() (string, error) { return good, nil }
	view, err := f.svc.AddFileSource("op", "本地节点")
	if err != nil {
		t.Fatal(err)
	}
	if view.Kind != model.ProxySourceKindFile {
		t.Fatalf("kind = %q", view.Kind)
	}
	// DeBrowser's own copy exists under data/mihomo/sources/ and
	// the original file is untouched.
	src, err := f.store.GetProxySource(context.Background(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Layout.Mihomo, "sources", src.FileName)); err != nil {
		t.Fatalf("stored copy missing: %v", err)
	}
	orig, _ := os.ReadFile(good)
	if !strings.Contains(string(orig), "name: A") {
		t.Fatal("original file must be untouched")
	}
	// Deleting the file source removes the copy.
	if err := f.svc.DeleteSource("op", src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.Layout.Mihomo, "sources", src.FileName)); !os.IsNotExist(err) {
		t.Fatalf("stored copy after delete: err = %v", err)
	}
}

// TestProxySecretConcurrentInit hammers the service's read paths
// from many goroutines on a fresh service: the controller secret
// must be generated exactly once, the secret file must hold the
// same value the client/manager were built with, and no data race
// may occur (run with -race).
func TestProxySecretConcurrentInit(t *testing.T) {
	f := testProxyFixture(t)
	const workers = 32
	errs := make(chan error, workers*4)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.svc.GetCoreStatus()
			if _, err := f.svc.ListSources(); err != nil {
				errs <- err
			}
			if _, err := f.svc.ListNodes(); err != nil {
				errs <- err
			}
			if _, err := f.svc.ListBindings(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent read: %v", err)
	}
	if f.svc.secret == "" {
		t.Fatal("secret was not initialized")
	}
	raw, err := os.ReadFile(filepath.Join(f.cfg.Layout.Mihomo, "controller-secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != f.svc.secret {
		t.Fatalf("secret file and in-memory secret differ")
	}
	if f.svc.manager == nil || f.svc.client == nil {
		t.Fatal("client/manager not initialized")
	}
	// A second service over the same layout loads the SAME secret
	// from the file instead of generating a new one.
	svc2 := NewProxyService(f.db, f.cfg.Layout, f.vault, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := svc2.initCore(); err != nil {
		t.Fatal(err)
	}
	if svc2.secret != f.svc.secret {
		t.Fatal("second service generated a different secret")
	}
}

// TestCoreNeverStartsImplicitly: with sources and bindings in
// place, reads and binding operations leave the core stopped —
// only StartCore (a manual user action) may start it.
func TestCoreNeverStartsImplicitly(t *testing.T) {
	f := testProxyFixture(t)
	view, err := f.svc.AddURLSource("op", "机场", testSubURL)
	if err != nil {
		t.Fatal(err)
	}
	inst := f.makeInstance(t, "实例")
	if _, err := f.svc.SetBinding("op", inst.ID, view.ID, "香港 01"); err != nil {
		t.Fatal(err)
	}
	if st := f.svc.GetCoreStatus(); st.State != "stopped" {
		t.Fatalf("core state after reads/writes = %q, want stopped (no autostart)", st.State)
	}
	if err := f.svc.RefreshSource("op", view.ID); err == nil {
		t.Fatal("refresh with a stopped core must fail clearly, not start the core")
	}
	if _, err := f.svc.TestDelays("op"); err == nil {
		t.Fatal("delay test with a stopped core must fail clearly, not start the core")
	}
	if st := f.svc.GetCoreStatus(); st.State != "stopped" {
		t.Fatalf("core state at end = %q, want stopped", st.State)
	}
}

// gateHealFixture builds one URL source and one instance bound
// to NodeA, entirely with the core stopped (the state the launch
// gate's healing logic starts from).
func gateHealFixture(t *testing.T) (*proxyFixture, model.InstanceProxyBinding) {
	t.Helper()
	f := testProxyFixture(t)
	src, err := f.svc.AddURLSource("op", "机场", testSubURL)
	if err != nil {
		t.Fatal(err)
	}
	inst := f.makeInstance(t, "实例一")
	if _, err := f.svc.SetBinding("op", inst.ID, src.ID, "NodeA"); err != nil {
		t.Fatal(err)
	}
	b, err := f.store.GetBinding(context.Background(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, b
}

func TestGateHealReappliesRecordedSelection(t *testing.T) {
	f, b := gateHealFixture(t)
	var selects []string
	f.svc.selectOverride = func(ctx context.Context, group, node string) error {
		selects = append(selects, group+"="+node)
		return nil
	}
	// After the re-application the core reports the group serving
	// its recorded node again: the heal succeeds. (The provider
	// read inside applySelections fails against the not-running
	// controller here, which only decides WHICH select is issued;
	// the groupNowOverride is the heal's verdict input.)
	f.svc.groupNowOverride = func(ctx context.Context, group string) (string, error) {
		return "NodeA", nil
	}
	if err := f.svc.reapplySelectionForBinding(context.Background(), b); err != nil {
		t.Fatalf("heal = %v, want nil", err)
	}
	if len(selects) == 0 {
		t.Fatal("the heal must re-apply the recorded selections (no select call recorded)")
	}
}

func TestGateHealRefusesWhenNodeStaysRejected(t *testing.T) {
	f, b := gateHealFixture(t)
	f.svc.selectOverride = func(ctx context.Context, group, node string) error { return nil }
	f.svc.groupNowOverride = func(ctx context.Context, group string) (string, error) {
		return "REJECT", nil
	}
	err := f.svc.reapplySelectionForBinding(context.Background(), b)
	if err == nil || !strings.Contains(err.Error(), "NodeA") {
		t.Fatalf("heal = %v, want a refusal naming the bound node", err)
	}
}

func TestGateHealSkipsGroupsThatAreNotRejected(t *testing.T) {
	f, b := gateHealFixture(t)
	selects := 0
	f.svc.selectOverride = func(ctx context.Context, group, node string) error {
		selects++
		return nil
	}
	// A group already serving its node: nothing to heal.
	f.svc.groupNowOverride = func(ctx context.Context, group string) (string, error) {
		return "NodeA", nil
	}
	if err := f.svc.healRejectedGroup(context.Background(), b); err != nil {
		t.Fatalf("healthy group heal = %v, want nil", err)
	}
	// A selection that cannot even be read (core hiccup) must not
	// block the launch either; the listener probe stays in charge.
	f.svc.groupNowOverride = func(ctx context.Context, group string) (string, error) {
		return "", errors.New("core hiccup")
	}
	if err := f.svc.healRejectedGroup(context.Background(), b); err != nil {
		t.Fatalf("unreadable group heal = %v, want nil", err)
	}
	if selects != 0 {
		t.Fatal("no re-application may happen for a healthy or unreadable group")
	}
}

func TestRefreshAfterInstanceDeletedWithoutCore(t *testing.T) {
	f := testProxyFixture(t)
	// Core stopped: the deletion hook is a no-op — above all it
	// must not start the core implicitly.
	f.svc.RefreshAfterInstanceDeleted("no-such-instance")
	if st := f.svc.GetCoreStatus(); st.State != "stopped" {
		t.Fatalf("core state after deletion hook = %q, want stopped", st.State)
	}
}
