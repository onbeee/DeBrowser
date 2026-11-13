package installstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/onbeee/DeBrowser/internal/paths"
)

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	return layout
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A brand-new folder (exe only) is uninitialized, and Detect must not
// create anything while finding that out.
func TestDetectUninitializedCreatesNothing(t *testing.T) {
	layout := testLayout(t)
	if got := Detect(layout); got != PhaseUninitialized {
		t.Fatalf("Detect = %v, want PhaseUninitialized", got)
	}
	entries, err := os.ReadDir(layout.Root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Detect created %d entries in a fresh root, want 0", len(entries))
	}
}

func TestDetectReadyByCompleteMarker(t *testing.T) {
	layout := testLayout(t)
	if err := MarkComplete(layout); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	if got := Detect(layout); got != PhaseReady {
		t.Fatalf("Detect = %v, want PhaseReady", got)
	}
	marker, exists, err := ReadMarker(layout)
	if err != nil || !exists {
		t.Fatalf("ReadMarker = (%v, %v), want marker present without error", exists, err)
	}
	if marker.State != StateComplete || marker.LayoutVersion != LayoutVersion {
		t.Fatalf("marker = %+v, want state=complete layoutVersion=%d", marker, LayoutVersion)
	}
}

// Installations created before the marker existed are recognized by
// their formal database file alone.
func TestDetectReadyByDatabaseOnly(t *testing.T) {
	layout := testLayout(t)
	writeFile(t, layout.DatabaseFile)
	if got := Detect(layout); got != PhaseReady {
		t.Fatalf("Detect = %v, want PhaseReady (database exists, no marker)", got)
	}
}

// The initializing marker is written before the database is created,
// so it wins over the database check: a folder with both is an
// interrupted initialization, not a ready installation.
func TestDetectInitializingMarkerWinsOverDatabase(t *testing.T) {
	layout := testLayout(t)
	if err := MarkInitializing(layout); err != nil {
		t.Fatalf("MarkInitializing: %v", err)
	}
	writeFile(t, layout.DatabaseFile)
	if got := Detect(layout); got != PhaseInitializing {
		t.Fatalf("Detect = %v, want PhaseInitializing", got)
	}
}

// A corrupt marker with no database still means an initialization was
// attempted and interrupted; with a database it must not block the
// normal startup of an existing installation.
func TestDetectCorruptMarker(t *testing.T) {
	layout := testLayout(t)
	writeFile(t, layout.InstallFile)
	if err := os.WriteFile(layout.InstallFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Detect(layout); got != PhaseInitializing {
		t.Fatalf("Detect (corrupt marker, no db) = %v, want PhaseInitializing", got)
	}
	writeFile(t, layout.DatabaseFile)
	if got := Detect(layout); got != PhaseReady {
		t.Fatalf("Detect (corrupt marker, db present) = %v, want PhaseReady", got)
	}
}

func TestCleanupFormalRestoresExeOnly(t *testing.T) {
	layout := testLayout(t)
	exe := filepath.Join(layout.Root, "DeBrowser.exe")
	writeFile(t, exe)
	writeFile(t, layout.DatabaseFile)
	writeFile(t, layout.SecretsFile)
	writeFile(t, layout.SettingsFile)
	if err := os.MkdirAll(layout.WebView2, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := MarkInitializing(layout); err != nil {
		t.Fatalf("MarkInitializing: %v", err)
	}

	if err := CleanupFormal(layout); err != nil {
		t.Fatalf("CleanupFormal: %v", err)
	}
	for _, dir := range []string{layout.Data, layout.Config, layout.WebView2} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("directory %s still exists after cleanup (err=%v)", dir, err)
		}
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("executable placeholder missing after cleanup: %v", err)
	}
	if got := Detect(layout); got != PhaseUninitialized {
		t.Fatalf("Detect after cleanup = %v, want PhaseUninitialized", got)
	}
	// Cleanup of an already-clean folder is a no-op success.
	if err := CleanupFormal(layout); err != nil {
		t.Fatalf("CleanupFormal on clean folder: %v", err)
	}
}

// The welcome workspace is deterministic per installation root (so a
// second launch finds the first one's lock), distinct across roots,
// lives under the OS temp directory, and creates nothing inside the
// portable folder itself.
func TestTempWorkspace(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()

	wsA1, err := TempWorkspace(rootA)
	if err != nil {
		t.Fatalf("TempWorkspace: %v", err)
	}
	defer func() { _ = os.RemoveAll(wsA1.Dir) }()
	wsA2, err := TempWorkspace(rootA)
	if err != nil {
		t.Fatalf("TempWorkspace again: %v", err)
	}
	if wsA1.Dir != wsA2.Dir {
		t.Fatalf("workspace not deterministic: %q vs %q", wsA1.Dir, wsA2.Dir)
	}
	wsB, err := TempWorkspace(rootB)
	if err != nil {
		t.Fatalf("TempWorkspace B: %v", err)
	}
	defer func() { _ = os.RemoveAll(wsB.Dir) }()
	if wsA1.Dir == wsB.Dir {
		t.Fatalf("different roots share a workspace: %q", wsA1.Dir)
	}
	if filepath.Dir(wsA1.Dir) != filepath.Join(os.TempDir(), "DeBrowser") {
		t.Fatalf("workspace %q is not under the OS temp directory", wsA1.Dir)
	}
	if info, err := os.Stat(wsA1.WebView2); err != nil || !info.IsDir() {
		t.Fatalf("webview2 temp dir missing: %v", err)
	}
	entries, err := os.ReadDir(rootA)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("TempWorkspace created %d entries in the portable root, want 0", len(entries))
	}
}
