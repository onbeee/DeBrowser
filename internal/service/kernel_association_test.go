package service

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Instance ↔ registered-browser association tests. Registrations
// are inserted through the store with paths that do not exist on
// disk: association, path resolution and removal guarding must
// never touch the file system (pure path registration semantics).

type assocFixture struct {
	inst  *InstanceService
	br    *BrowserService
	store *store.Store
}

func testAssocFixture(t *testing.T) *assocFixture {
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
	inst, err := NewInstanceService(db, cfg.Layout, slog.Default())
	if err != nil {
		t.Fatalf("instance service: %v", err)
	}
	return &assocFixture{
		inst:  inst,
		br:    NewBrowserService(db, cfg.Layout, slog.Default()),
		store: store.New(db),
	}
}

func (fx *assocFixture) register(t *testing.T, id, name, path string) {
	t.Helper()
	if _, err := fx.store.CreateExternalBrowser(t.Context(), model.ExternalBrowser{
		ID: id, Name: name, ExecutablePath: path,
	}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func TestInstanceKernelAssociationLifecycle(t *testing.T) {
	fx := testAssocFixture(t)
	fx.register(t, "reg-a", "brave", `C:\Browsers\a\brave.exe`)
	fx.register(t, "reg-b", "chrome", `C:\Browsers\b\chrome.exe`)

	// No association chosen: everything stays empty.
	plain, err := fx.inst.CreateInstance("", CreateInstanceInput{Name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.KernelID != "" || plain.KernelName != "" || plain.KernelExecutablePath != "" {
		t.Fatalf("unassociated instance carries kernel data: %+v", plain)
	}

	// Association by ID at creation: ID + name snapshot stored,
	// path resolved live from the registration.
	linked, err := fx.inst.CreateInstance("", CreateInstanceInput{Name: "linked", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if linked.KernelID != "reg-a" || linked.KernelName != "brave" || linked.KernelVersion != "" {
		t.Fatalf("association stored wrong: %+v", linked)
	}
	if linked.KernelExecutablePath != `C:\Browsers\a\brave.exe` {
		t.Fatalf("resolved path = %q", linked.KernelExecutablePath)
	}

	// Switching the association on update.
	updated, err := fx.inst.UpdateInstance("", UpdateInstanceInput{ID: linked.ID, Name: "linked", BrowserID: "reg-b"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.KernelID != "reg-b" || updated.KernelName != "chrome" || updated.KernelExecutablePath != `C:\Browsers\b\chrome.exe` {
		t.Fatalf("association switch wrong: %+v", updated)
	}

	// The empty choice clears an existing association.
	cleared, err := fx.inst.UpdateInstance("", UpdateInstanceInput{ID: linked.ID, Name: "linked", BrowserID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.KernelID != "" || cleared.KernelName != "" || cleared.KernelVersion != "" || cleared.KernelExecutablePath != "" {
		t.Fatalf("association not cleared: %+v", cleared)
	}

	// A nonexistent registration ID is an explicit error, and no
	// instance is created from it.
	if _, err := fx.inst.CreateInstance("", CreateInstanceInput{Name: "bad", BrowserID: "reg-missing"}); err == nil ||
		!strings.Contains(err.Error(), "登记不存在") {
		t.Fatalf("missing registration error = %v", err)
	}
	list, err := fx.inst.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("instances = %d, want 2 (failed create must not persist)", len(list))
	}
}

func TestInstanceKernelAssociationDuplicateNames(t *testing.T) {
	fx := testAssocFixture(t)
	// Same display name, different IDs and paths: association must
	// follow the ID, never the name.
	fx.register(t, "reg-1", "chrome", `D:\one\chrome.exe`)
	fx.register(t, "reg-2", "chrome", `D:\two\chrome.exe`)

	inst, err := fx.inst.CreateInstance("", CreateInstanceInput{Name: "dup", BrowserID: "reg-2"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.KernelID != "reg-2" || inst.KernelExecutablePath != `D:\two\chrome.exe` {
		t.Fatalf("duplicate-name association resolved wrong: %+v", inst)
	}
	got, err := fx.inst.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KernelExecutablePath != `D:\two\chrome.exe` {
		t.Fatalf("GetInstance path = %q", got.KernelExecutablePath)
	}
}

func TestInstanceKernelPathFollowsRegistration(t *testing.T) {
	fx := testAssocFixture(t)
	fx.register(t, "reg-a", "brave", `C:\old\brave.exe`)
	inst, err := fx.inst.CreateInstance("", CreateInstanceInput{Name: "follow", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	// Re-selecting the registration's path (to a location that does
	// not exist — pure path semantics, no existence check) is
	// reflected on the associated instance immediately.
	fx.br.ExecutablePicker = func() (string, error) { return `C:\new\brave.exe`, nil }
	if _, err := fx.br.UpdateBrowserPath("", "reg-a"); err != nil {
		t.Fatal(err)
	}
	got, err := fx.inst.GetInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KernelExecutablePath != `C:\new\brave.exe` {
		t.Fatalf("instance still resolves the old path: %q", got.KernelExecutablePath)
	}
	if got.KernelID != "reg-a" || got.KernelName != "brave" {
		t.Fatalf("association disturbed by path change: %+v", got)
	}
}

func TestRemoveRegistrationInUseGuard(t *testing.T) {
	fx := testAssocFixture(t)
	fx.register(t, "reg-a", "brave", `C:\Browsers\a\brave.exe`)
	inst, err := fx.inst.CreateInstance("", CreateInstanceInput{Name: "guarded", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}

	// Referenced registration: removal refused with a clear
	// message; registration and instance both survive untouched.
	err = fx.br.RemoveBrowserRegistration("", "reg-a")
	if err == nil || !strings.Contains(err.Error(), "正被 1 个浏览器实例使用") {
		t.Fatalf("removal guard error = %v", err)
	}
	browsers, err := fx.br.ListBrowsers()
	if err != nil || len(browsers) != 1 {
		t.Fatalf("registration lost after refused removal: %v %+v", err, browsers)
	}
	got, err := fx.inst.GetInstance(inst.ID)
	if err != nil || got.KernelID != "reg-a" || got.KernelExecutablePath == "" {
		t.Fatalf("instance damaged by refused removal: %v %+v", err, got)
	}

	// After the association is cleared, removal succeeds and the
	// instance itself is NOT cascade-deleted.
	if _, err := fx.inst.UpdateInstance("", UpdateInstanceInput{ID: inst.ID, Name: "guarded", BrowserID: ""}); err != nil {
		t.Fatal(err)
	}
	if err := fx.br.RemoveBrowserRegistration("", "reg-a"); err != nil {
		t.Fatalf("removal after clearing association: %v", err)
	}
	if _, err := fx.inst.GetInstance(inst.ID); err != nil {
		t.Fatalf("instance was cascade-deleted: %v", err)
	}
}

func TestInstanceHistoricalKernelTextPreserved(t *testing.T) {
	fx := testAssocFixture(t)
	// A pre-association instance with free-text kernel name/version
	// recorded directly in the store (the historical shape).
	legacy, err := fx.store.CreateInstance(t.Context(), model.Instance{
		Name: "legacy", UserDataDir: "profiles/legacy",
		KernelName: "Chromium", KernelVersion: "130",
	})
	if err != nil {
		t.Fatal(err)
	}
	// An unrelated edit with no kernel choice preserves the
	// historical text verbatim.
	updated, err := fx.inst.UpdateInstance("", UpdateInstanceInput{ID: legacy.ID, Name: "legacy-renamed", BrowserID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if updated.KernelName != "Chromium" || updated.KernelVersion != "130" || updated.KernelID != "" {
		t.Fatalf("historical kernel text not preserved: %+v", updated)
	}
	// Choosing a registration replaces the text with the
	// association (name snapshot; version cleared — registrations
	// carry no version).
	fx.register(t, "reg-a", "brave", `C:\Browsers\a\brave.exe`)
	linked, err := fx.inst.UpdateInstance("", UpdateInstanceInput{ID: legacy.ID, Name: "legacy-renamed", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if linked.KernelID != "reg-a" || linked.KernelName != "brave" || linked.KernelVersion != "" {
		t.Fatalf("association over legacy text wrong: %+v", linked)
	}
}
