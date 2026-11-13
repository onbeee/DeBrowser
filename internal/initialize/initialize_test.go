package initialize

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/settings"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	return layout
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	return entries
}

func TestNewUserSuccess(t *testing.T) {
	layout := testLayout(t)
	// The exe-only starting point.
	exe := filepath.Join(layout.Root, "DeBrowser.exe")
	if err := os.WriteFile(exe, []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := NewUser(layout, testLogger()); err != nil {
		t.Fatalf("NewUser: %v", err)
	}

	// Marker: complete, current layout version.
	marker, exists, err := installstate.ReadMarker(layout)
	if err != nil || !exists {
		t.Fatalf("ReadMarker = (exists=%v, err=%v), want present", exists, err)
	}
	if marker.State != installstate.StateComplete {
		t.Fatalf("marker state = %q, want %q", marker.State, installstate.StateComplete)
	}
	if got := installstate.Detect(layout); got != installstate.PhaseReady {
		t.Fatalf("Detect = %v, want PhaseReady", got)
	}

	// Formal structure per paths.Layout — the original paths, not
	// redesigned ones.
	for _, dir := range []string{layout.Data, layout.Backups, layout.Logs, layout.Profiles, layout.Mihomo, layout.Config, layout.WebView2} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("expected directory %s (err=%v)", dir, err)
		}
	}
	if _, err := os.Stat(layout.DatabaseFile); err != nil {
		t.Fatalf("database file missing: %v", err)
	}

	// Database: opened, fully migrated, integrity-clean.
	db, err := database.OpenWithOptions(layout.DatabaseFile, database.Options{BackupDir: layout.Backups})
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer func() { _ = db.Close() }()
	version, err := db.AppliedVersion(context.Background())
	if err != nil {
		t.Fatalf("AppliedVersion: %v", err)
	}
	if version != 5 {
		t.Fatalf("AppliedVersion = %d, want 5 (all migrations applied)", version)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, err=%v", integrity, err)
	}

	// Vault opens cleanly; settings hold the defaults.
	if _, _, err := secrets.Open(layout.SecretsFile); err != nil {
		t.Fatalf("open vault after init: %v", err)
	}
	got, err := settings.Load(layout.SettingsFile)
	if err != nil {
		t.Fatalf("load settings after init: %v", err)
	}
	if want := settings.Default(); got != want {
		t.Fatalf("settings = %+v, want defaults %+v", got, want)
	}
}

// A second startup must recognize the initialized installation and
// leave existing data untouched (no re-initialization, no overwrite).
func TestSecondStartupKeepsData(t *testing.T) {
	layout := testLayout(t)
	if err := NewUser(layout, testLogger()); err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	db, err := database.OpenWithOptions(layout.DatabaseFile, database.Options{BackupDir: layout.Backups})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := db.SQL().ExecContext(context.Background(),
		`INSERT INTO app_settings (key, value) VALUES ('stage5a_sentinel', 'keep-me')`); err != nil {
		t.Fatalf("insert sentinel: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	if got := installstate.Detect(layout); got != installstate.PhaseReady {
		t.Fatalf("Detect = %v, want PhaseReady", got)
	}
	db2, err := database.OpenWithOptions(layout.DatabaseFile, database.Options{BackupDir: layout.Backups})
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if err := db2.Migrate(context.Background()); err != nil {
		t.Fatalf("second-startup Migrate: %v", err)
	}
	var value string
	if err := db2.SQL().QueryRowContext(context.Background(),
		`SELECT value FROM app_settings WHERE key = 'stage5a_sentinel'`).Scan(&value); err != nil {
		t.Fatalf("sentinel row lost on second startup: %v", err)
	}
	if value != "keep-me" {
		t.Fatalf("sentinel value = %q, want keep-me", value)
	}
}

// Failure injection: the data path is blocked by a regular file, so
// directory creation fails at the very first step. Nothing else may
// be created.
func TestNewUserFailureDirectoryBlocked(t *testing.T) {
	layout := testLayout(t)
	if err := os.WriteFile(layout.Data, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := NewUser(layout, testLogger())
	if err == nil {
		t.Fatal("NewUser succeeded despite a blocked data path")
	}
	if !strings.Contains(err.Error(), "记录初始化开始") {
		t.Fatalf("error = %q, want it to name the 记录初始化开始 step (recording the start creates the data directory)", err)
	}
	entries := dirEntries(t, layout.Root)
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("root entries = %v, want only the pre-existing blocking file", entries)
	}
}

// Failure injection: the database path is a directory, so opening the
// database fails after directories and the marker were created. The
// run's own artifacts must be cleaned; the pre-existing planted
// directory must remain (cleanup removes only what the run created).
func TestNewUserFailureDatabaseBlocked(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(layout.DatabaseFile, 0o750); err != nil {
		t.Fatal(err)
	}

	err := NewUser(layout, testLogger())
	if err == nil {
		t.Fatal("NewUser succeeded despite a blocked database path")
	}
	if !strings.Contains(err.Error(), "打开数据库") {
		t.Fatalf("error = %q, want it to name the 打开数据库 step", err)
	}
	if _, err := os.Stat(layout.InstallFile); !os.IsNotExist(err) {
		t.Fatalf("install marker survived a failed initialization (err=%v)", err)
	}
	for _, dir := range []string{layout.Config, layout.WebView2} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("directory %s survived a failed initialization (err=%v)", dir, err)
		}
	}
	entries := dirEntries(t, layout.Data)
	if len(entries) != 1 || entries[0].Name() != "debrowser.db" {
		t.Fatalf("data entries = %v, want only the pre-existing planted directory", entries)
	}
	// The leftover husk (a data directory with no marker and no
	// database file) is classified as an interrupted initialization,
	// and the startup recovery cleanup restores exe-only.
	if got := installstate.Detect(layout); got != installstate.PhaseInitializing {
		t.Fatalf("Detect = %v, want PhaseInitializing for the leftover residue", got)
	}
	if err := installstate.CleanupFormal(layout); err != nil {
		t.Fatalf("CleanupFormal: %v", err)
	}
	if got := installstate.Detect(layout); got != installstate.PhaseUninitialized {
		t.Fatalf("Detect = %v, want PhaseUninitialized after recovery cleanup", got)
	}
}

// Failure injection: the vault path is a directory, so vault
// initialization fails after the database was fully migrated. All
// artifacts of the run (database, marker, settings, directories)
// must be cleaned back to the pre-run state.
func TestNewUserFailureVaultBlocked(t *testing.T) {
	layout := testLayout(t)
	if err := os.MkdirAll(layout.SecretsFile, 0o750); err != nil {
		t.Fatal(err)
	}

	err := NewUser(layout, testLogger())
	if err == nil {
		t.Fatal("NewUser succeeded despite a blocked vault path")
	}
	if !strings.Contains(err.Error(), "凭据库") {
		t.Fatalf("error = %q, want it to name the vault step", err)
	}
	if _, err := os.Stat(layout.DatabaseFile); !os.IsNotExist(err) {
		t.Fatalf("database file survived a failed initialization (err=%v)", err)
	}
	if _, err := os.Stat(layout.InstallFile); !os.IsNotExist(err) {
		t.Fatalf("install marker survived a failed initialization (err=%v)", err)
	}
	if _, err := os.Stat(layout.SettingsFile); !os.IsNotExist(err) {
		t.Fatalf("settings file survived a failed initialization (err=%v)", err)
	}
	entries := dirEntries(t, layout.Data)
	if len(entries) != 1 || entries[0].Name() != "secrets.vault" {
		t.Fatalf("data entries = %v, want only the pre-existing planted directory", entries)
	}
	if got := installstate.Detect(layout); got != installstate.PhaseInitializing {
		t.Fatalf("Detect = %v, want PhaseInitializing for the leftover residue", got)
	}
	if err := installstate.CleanupFormal(layout); err != nil {
		t.Fatalf("CleanupFormal: %v", err)
	}
	if got := installstate.Detect(layout); got != installstate.PhaseUninitialized {
		t.Fatalf("Detect = %v, want PhaseUninitialized after recovery cleanup", got)
	}
}

// Interruption: an initialization that died midway leaves the
// initializing marker plus partial artifacts. Detect classifies it,
// CleanupFormal removes the artifacts, and the installation is back
// to exe-only, ready to show the welcome page again.
func TestInterruptedInitializationRecovery(t *testing.T) {
	layout := testLayout(t)
	exe := filepath.Join(layout.Root, "DeBrowser.exe")
	if err := os.WriteFile(exe, []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := installstate.MarkInitializing(layout); err != nil {
		t.Fatalf("MarkInitializing: %v", err)
	}
	// A partial artifact, as a crashed database creation would leave.
	if err := os.WriteFile(layout.DatabaseFile, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := installstate.Detect(layout); got != installstate.PhaseInitializing {
		t.Fatalf("Detect = %v, want PhaseInitializing", got)
	}
	if err := installstate.CleanupFormal(layout); err != nil {
		t.Fatalf("CleanupFormal: %v", err)
	}
	entries := dirEntries(t, layout.Root)
	if len(entries) != 1 || entries[0].Name() != "DeBrowser.exe" {
		t.Fatalf("root entries = %v, want only the executable", entries)
	}
	if got := installstate.Detect(layout); got != installstate.PhaseUninitialized {
		t.Fatalf("Detect = %v, want PhaseUninitialized", got)
	}
	// And a fresh initialization succeeds afterwards.
	if err := NewUser(layout, testLogger()); err != nil {
		t.Fatalf("NewUser after recovery: %v", err)
	}
	if got := installstate.Detect(layout); got != installstate.PhaseReady {
		t.Fatalf("Detect = %v, want PhaseReady", got)
	}
}
