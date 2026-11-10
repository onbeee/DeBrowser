package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertUnder(t *testing.T, root, p string) {
	t.Helper()
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Errorf("path %s escapes root %s", p, root)
	}
}

func TestResolveLayoutUnderRoot(t *testing.T) {
	// Two different roots stand in for launching from a D: folder and
	// from an E: folder: the layout must follow the given root exactly.
	for _, root := range []string{filepath.Join(t.TempDir(), "drive-d", "DeBrowser"), filepath.Join(t.TempDir(), "drive-e", "DeBrowser")} {
		l, err := Resolve(root)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", root, err)
		}
		for _, p := range []string{l.Data, l.DatabaseFile, l.Backups, l.Logs, l.LogFile, l.Profiles, l.Mihomo, l.WebView2, l.Config, l.SettingsFile} {
			assertUnder(t, l.Root, p)
		}
		if filepath.Base(l.DatabaseFile) != "debrowser.db" {
			t.Errorf("database file = %s", l.DatabaseFile)
		}
		if filepath.Base(l.WebView2) != "webview2-data" {
			t.Errorf("webview2 dir = %s", l.WebView2)
		}
		if filepath.Base(l.SettingsFile) != "settings.json" {
			t.Errorf("settings file = %s", l.SettingsFile)
		}
	}
}

func TestResolveIndependentOfWorkingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "DeBrowser")
	before, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	after, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("layout changed with working directory:\nbefore=%+v\nafter =%+v", before, after)
	}
}

func TestEnsureCreatesAndProbes(t *testing.T) {
	l, err := Resolve(filepath.Join(t.TempDir(), "DeBrowser"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for _, dir := range []string{l.Data, l.Backups, l.Logs, l.Profiles, l.Mihomo, l.WebView2, l.Config} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Errorf("directory %s not created: %v", dir, err)
		}
	}
	// Probe files must not be left behind.
	entries, err := os.ReadDir(l.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "write-probe") {
			t.Errorf("probe file left behind: %s", e.Name())
		}
	}
}

func TestEnsureReportsUnwritableRoot(t *testing.T) {
	root := t.TempDir()
	// A regular file where the data directory must be created makes the
	// layout impossible to create on any platform.
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err == nil {
		t.Fatal("Ensure succeeded despite blocked data directory")
	}
}
