package service

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
)

// Manual-registration tests. The managed downloadable-kernel
// feature (and its tests) was removed by product decision
// (2026-11-15); what remains is the pure path registration.

type browserFixture struct {
	svc        *BrowserService
	db         *database.DB
	cfg        config.Config
	chromePath string
	bravePath  string
}

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testBrowserFixture(t *testing.T) *browserFixture {
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
	fx := &browserFixture{db: db, cfg: cfg}
	fx.chromePath = filepath.Join(cfg.Layout.Root, "installed", "chrome.exe")
	fx.bravePath = filepath.Join(cfg.Layout.Root, "installed", "brave.exe")
	writeExe(t, fx.chromePath)
	writeExe(t, fx.bravePath)
	fx.svc = fx.newService()
	return fx
}

// newService builds a service over the same database — a second
// call simulates an application restart.
func (fx *browserFixture) newService() *BrowserService {
	return NewBrowserService(fx.db, fx.cfg.Layout, slog.Default())
}

func TestNoAutoDetection(t *testing.T) {
	fx := testBrowserFixture(t)
	// Browsers exist on disk, but without an explicit user action
	// nothing is listed: DeBrowser never auto-detects.
	got, err := fx.svc.ListBrowsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ListBrowsers without any user action = %+v, want empty", got)
	}
}

func TestManualAddPersistsAcrossRestart(t *testing.T) {
	fx := testBrowserFixture(t)
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	added, err := fx.svc.AddBrowserManually("")
	if err != nil {
		t.Fatalf("AddBrowserManually: %v", err)
	}
	// Pure path registration: the view carries the chosen path and
	// a display name derived from the file name — and nothing else
	// (no kind, version, signature or verification fields).
	if added.Source != "manual" || added.Name != "chrome" || added.ExecutablePath != fx.chromePath {
		t.Fatalf("added = %+v", added)
	}
	// (The view type itself carries no kind, version, signature
	// or verification fields at all — there is nothing to check.)
	// The browser file must NOT be copied into DeBrowser's folders.
	if strings.Contains(added.ExecutablePath, fx.cfg.Layout.Kernels) || strings.Contains(added.ExecutablePath, fx.cfg.Layout.Data) {
		t.Fatalf("browser path points inside DeBrowser data: %s", added.ExecutablePath)
	}
	// A fresh service over the same DB (simulated restart) still
	// lists the registration, read back from SQLite unchanged.
	restarted := fx.newService()
	got, err := restarted.ListBrowsers()
	if err != nil || len(got) != 1 || got[0].ID != added.ID || got[0].ExecutablePath != fx.chromePath {
		t.Fatalf("after restart ListBrowsers = %+v, %v", got, err)
	}
	details, err := restarted.GetBrowserDetails(added.ID)
	if err != nil || details.ExecutablePath != fx.chromePath {
		t.Fatalf("GetBrowserDetails = %+v, %v", details, err)
	}
	// Adding the same path again returns the same registration,
	// not a duplicate.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	again, err := fx.svc.AddBrowserManually("")
	if err != nil || again.ID != added.ID {
		t.Fatalf("duplicate add = %+v, %v", again, err)
	}
	got, _ = fx.svc.ListBrowsers()
	if len(got) != 1 {
		t.Fatalf("duplicate add produced %d records", len(got))
	}
	// Any other program can be added the same way.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.bravePath, nil }
	brave, err := fx.svc.AddBrowserManually("")
	if err != nil || brave.Name != "brave" {
		t.Fatalf("brave add = %+v, %v", brave, err)
	}
}

// TestManualAddAcceptsAnyExecutable proves the manual flow
// performs NO identity or security checks: files whose name,
// product identity, signature or publisher would previously have
// been rejected — and even a path that does not exist on disk at
// all (so no inspection of it could possibly have happened) —
// are all registered as plain paths.
func TestManualAddAcceptsAnyExecutable(t *testing.T) {
	fx := testBrowserFixture(t)
	mk := func(name string) string {
		p := filepath.Join(fx.cfg.Layout.Root, "other", name)
		writeExe(t, p)
		return p
	}
	paths := []string{
		mk("chromium.exe"),       // a Chromium-named program
		mk("fake-chrome.exe"),    // any name at all
		mk("unsigned-tool.exe"),  // unsigned, unknown product
		mk("totally-random.exe"), // garbage content, random name
		filepath.Join(fx.cfg.Layout.Root, "other", "never-existed.exe"), // not on disk
	}
	for _, p := range paths {
		path := p
		fx.svc.ExecutablePicker = func() (string, error) { return path, nil }
		v, err := fx.svc.AddBrowserManually("")
		if err != nil {
			t.Fatalf("AddBrowserManually(%s): %v", path, err)
		}
		if v.ExecutablePath != path || v.Source != "manual" {
			t.Fatalf("registered view = %+v, want path %s", v, path)
		}
	}
	got, _ := fx.svc.ListBrowsers()
	if len(got) != len(paths) {
		t.Fatalf("registered %d browsers, want %d", len(got), len(paths))
	}
	// Picker cancelled: no record.
	fx2 := testBrowserFixture(t)
	fx2.svc.ExecutablePicker = func() (string, error) { return "", nil }
	if _, err := fx2.svc.AddBrowserManually(""); err == nil {
		t.Fatal("cancelled picker must error")
	}
	got, _ = fx2.svc.ListBrowsers()
	if len(got) != 0 {
		t.Fatalf("cancelled picker left records: %+v", got)
	}
}

func TestUpdateBrowserPath(t *testing.T) {
	fx := testBrowserFixture(t)
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	added, err := fx.svc.AddBrowserManually("")
	if err != nil {
		t.Fatal(err)
	}
	// Re-select a different path: the record updates in place.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.bravePath, nil }
	updated, err := fx.svc.UpdateBrowserPath("", added.ID)
	if err != nil {
		t.Fatalf("UpdateBrowserPath: %v", err)
	}
	if updated.ID != added.ID || updated.ExecutablePath != fx.bravePath || updated.Name != "brave" {
		t.Fatalf("updated = %+v", updated)
	}
	// The update persists across a simulated restart.
	restarted := fx.newService()
	got, _ := restarted.ListBrowsers()
	if len(got) != 1 || got[0].ExecutablePath != fx.bravePath {
		t.Fatalf("after restart = %+v", got)
	}
	// A path already held by another registration cannot be taken.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	other, err := fx.svc.AddBrowserManually("")
	if err != nil {
		t.Fatal(err)
	}
	fx.svc.ExecutablePicker = func() (string, error) { return fx.bravePath, nil }
	if _, err := fx.svc.UpdateBrowserPath("", other.ID); err == nil {
		t.Fatal("taking another registration's path must fail")
	}
	// Unknown registration ID errors.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	if _, err := fx.svc.UpdateBrowserPath("", "no-such-id"); err == nil {
		t.Fatal("updating a missing registration must error")
	}
	// The registered programs themselves were never launched,
	// moved or modified by any of this: both files still exist
	// with their original content.
	for _, p := range []string{fx.chromePath, fx.bravePath} {
		body, err := os.ReadFile(p)
		if err != nil || string(body) != "MZ" {
			t.Fatalf("registered file changed: %s (%v)", p, err)
		}
	}
}

func TestRemoveRegistrationKeepsBrowserFile(t *testing.T) {
	fx := testBrowserFixture(t)
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	added, err := fx.svc.AddBrowserManually("")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RemoveBrowserRegistration("", added.ID); err != nil {
		t.Fatalf("RemoveBrowserRegistration: %v", err)
	}
	// The browser binary itself must be completely untouched.
	if _, err := os.Stat(fx.chromePath); err != nil {
		t.Fatalf("browser file was touched by registration removal: %v", err)
	}
	got, _ := fx.svc.ListBrowsers()
	if len(got) != 0 {
		t.Fatalf("registration should be gone: %+v", got)
	}
	if err := fx.svc.RemoveBrowserRegistration("", added.ID); err == nil {
		t.Fatal("removing a missing registration must error")
	}
}
