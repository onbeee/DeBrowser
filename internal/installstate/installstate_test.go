package installstate

import (
	"errors"
	"fmt"
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

// Interruption before the marker could be recorded: only formal
// residue exists (an empty data directory, a config directory, a
// webview2-data directory, or a torn marker temp file). Detect must
// classify it as an interrupted initialization — never as ready and
// never as a plain uninitialized folder — and CleanupFormal must
// restore exe-only.
func TestDetectResidueWithoutMarker(t *testing.T) {
	cases := map[string]func(layout paths.Layout){
		"data dir only": func(layout paths.Layout) {
			if err := os.MkdirAll(layout.Data, 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"config dir only": func(layout paths.Layout) {
			if err := os.MkdirAll(layout.Config, 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"webview2 dir only": func(layout paths.Layout) {
			if err := os.MkdirAll(layout.WebView2, 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"torn marker temp file": func(layout paths.Layout) {
			writeFile(t, filepath.Join(layout.Data, ".install-123456.tmp"))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			layout := testLayout(t)
			exe := filepath.Join(layout.Root, "DeBrowser.exe")
			writeFile(t, exe)
			plant(layout)
			if got := Detect(layout); got != PhaseInitializing {
				t.Fatalf("Detect = %v, want PhaseInitializing (interrupted residue)", got)
			}
			if err := CleanupFormal(layout); err != nil {
				t.Fatalf("CleanupFormal: %v", err)
			}
			entries, err := os.ReadDir(layout.Root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "DeBrowser.exe" {
				t.Fatalf("root entries = %v, want exe-only after recovery", entries)
			}
			if got := Detect(layout); got != PhaseUninitialized {
				t.Fatalf("Detect after recovery = %v, want PhaseUninitialized", got)
			}
		})
	}
}

// Interruption at the vault stage: marker written, database created,
// vault file present but initialization never completed. The marker
// still classifies it as interrupted and recovery restores exe-only.
func TestInterruptedAtVaultStage(t *testing.T) {
	layout := testLayout(t)
	writeFile(t, filepath.Join(layout.Root, "DeBrowser.exe"))
	if err := MarkInitializing(layout); err != nil {
		t.Fatalf("MarkInitializing: %v", err)
	}
	writeFile(t, layout.DatabaseFile)
	writeFile(t, layout.SecretsFile)
	if got := Detect(layout); got != PhaseInitializing {
		t.Fatalf("Detect = %v, want PhaseInitializing", got)
	}
	if err := CleanupFormal(layout); err != nil {
		t.Fatalf("CleanupFormal: %v", err)
	}
	if got := Detect(layout); got != PhaseUninitialized {
		t.Fatalf("Detect after recovery = %v, want PhaseUninitialized", got)
	}
}

func writeMarkerRaw(t *testing.T, layout paths.Layout, state string, version int) {
	t.Helper()
	if err := os.MkdirAll(layout.Data, 0o750); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"state":%q,"layoutVersion":%d,"updatedAt":"2026-11-13T00:00:00Z"}`, state, version))
	if err := os.WriteFile(layout.InstallFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Layout-version compatibility: a completed marker at the supported
// version starts normally; a higher or lower version is refused
// (never treated as a new installation); a legacy database-only
// installation passes and is detected ready.
func TestCheckLayoutCompatibility(t *testing.T) {
	t.Run("current version passes", func(t *testing.T) {
		layout := testLayout(t)
		writeMarkerRaw(t, layout, StateComplete, LayoutVersion)
		if err := CheckLayoutCompatibility(layout); err != nil {
			t.Fatalf("CheckLayoutCompatibility: %v", err)
		}
		if got := Detect(layout); got != PhaseReady {
			t.Fatalf("Detect = %v, want PhaseReady", got)
		}
	})
	t.Run("newer version refused", func(t *testing.T) {
		layout := testLayout(t)
		writeMarkerRaw(t, layout, StateComplete, LayoutVersion+1)
		err := CheckLayoutCompatibility(layout)
		var incompat *IncompatibleLayoutError
		if !errors.As(err, &incompat) {
			t.Fatalf("error = %v, want IncompatibleLayoutError", err)
		}
		if incompat.Found != LayoutVersion+1 || incompat.Supported != LayoutVersion {
			t.Fatalf("incompat = %+v, want Found=%d Supported=%d", incompat, LayoutVersion+1, LayoutVersion)
		}
		// Still classified ready (startup refuses with the version
		// error) — it must NOT fall back to the welcome flow.
		if got := Detect(layout); got != PhaseReady {
			t.Fatalf("Detect = %v, want PhaseReady", got)
		}
	})
	t.Run("older version refused", func(t *testing.T) {
		layout := testLayout(t)
		writeMarkerRaw(t, layout, StateComplete, 0)
		err := CheckLayoutCompatibility(layout)
		var incompat *IncompatibleLayoutError
		if !errors.As(err, &incompat) {
			t.Fatalf("error = %v, want IncompatibleLayoutError", err)
		}
		if incompat.Found != 0 {
			t.Fatalf("incompat.Found = %d, want 0", incompat.Found)
		}
	})
	t.Run("no marker (legacy database install) passes", func(t *testing.T) {
		layout := testLayout(t)
		writeFile(t, layout.DatabaseFile)
		if err := CheckLayoutCompatibility(layout); err != nil {
			t.Fatalf("CheckLayoutCompatibility: %v", err)
		}
		if got := Detect(layout); got != PhaseReady {
			t.Fatalf("Detect = %v, want PhaseReady", got)
		}
	})
	t.Run("initializing marker not version-checked", func(t *testing.T) {
		layout := testLayout(t)
		writeMarkerRaw(t, layout, StateInitializing, LayoutVersion+5)
		if err := CheckLayoutCompatibility(layout); err != nil {
			t.Fatalf("CheckLayoutCompatibility = %v, want nil (recovery path)", err)
		}
		if got := Detect(layout); got != PhaseInitializing {
			t.Fatalf("Detect = %v, want PhaseInitializing", got)
		}
	})
}
