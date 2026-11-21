package service

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

// Stage 6C proxy service tests. The core itself is never started:
// coreStarterOverride stands in for it, and every assertion runs
// against the database, the vault and the generated configuration
// file — the artifacts DeBrowser actually owns.

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
	svc.coreStarterOverride = func(ctx context.Context) error { return nil }
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

	b1, err := f.svc.SetBinding("op", inst1.ID, src.ID, "香港 01", model.ProxyModeRule)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Port != proxyListenerBasePort {
		t.Fatalf("first binding port = %d, want %d", b1.Port, proxyListenerBasePort)
	}
	b2, err := f.svc.SetBinding("op", inst2.ID, src.ID, "日本 01", model.ProxyModeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Port != proxyListenerBasePort+1 {
		t.Fatalf("second binding port = %d, want %d", b2.Port, proxyListenerBasePort+1)
	}
	// Re-binding keeps the instance's port.
	b1again, err := f.svc.SetBinding("op", inst1.ID, src.ID, "香港 02", model.ProxyModeRule)
	if err != nil {
		t.Fatal(err)
	}
	if b1again.Port != b1.Port || b1again.NodeName != "香港 02" {
		t.Fatalf("rebind = %+v", b1again)
	}
	if got := f.svc.ProxyPortForInstance(inst1.ID); got != b1.Port {
		t.Fatalf("ProxyPortForInstance = %d, want %d", got, b1.Port)
	}
	if got := f.svc.ProxyPortForInstance("no-such-instance"); got != 0 {
		t.Fatalf("unbound instance port = %d, want 0", got)
	}

	// The generated configuration carries the whole wiring: the
	// subscription URL reaches only this file (inside the core
	// working directory), listeners and sub-rules are per instance.
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
		"MATCH,inst-",
		"proxy: DIRECT",
	} {
		if !strings.Contains(cfgText, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, cfgText)
		}
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
	if _, err := f.svc.SetBinding("op", inst.ID, "no-source", "N", model.ProxyModeRule); err == nil {
		t.Error("unknown source must fail")
	}
	view, err := f.svc.AddURLSource("op", "机场", testSubURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetBinding("op", "no-instance", view.ID, "N", model.ProxyModeRule); err == nil {
		t.Error("unknown instance must fail")
	}
	if _, err := f.svc.SetBinding("op", inst.ID, view.ID, "", model.ProxyModeRule); err == nil {
		t.Error("empty node must fail")
	}
	if _, err := f.svc.SetBinding("op", inst.ID, view.ID, "N", "weird"); err == nil {
		t.Error("invalid mode must fail")
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
