package migration

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/paths"
)

func makeLegacyDB(t *testing.T, file string) {
	t.Helper()
	db, err := sql.Open("sqlite", database.FileURI(file))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	stmts := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL)",
		"INSERT INTO app_settings VALUES ('theme', 'warm', '2026-11-10T00:00:00Z')",
		"CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)",
		"INSERT INTO schema_migrations VALUES (1, 'foundation', '2026-11-10T00:00:00Z')",
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("legacy setup %q: %v", s, err)
		}
	}
}

func readTheme(t *testing.T, file string) string {
	t.Helper()
	db, err := sql.Open("sqlite", database.FileURI(file)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow("SELECT value FROM app_settings WHERE key='theme'").Scan(&v); err != nil {
		t.Fatalf("read theme from %s: %v", file, err)
	}
	return v
}

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	l, err := paths.Resolve(filepath.Join(t.TempDir(), "DeBrowser-portable"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestMigrateLegacyData(t *testing.T) {
	legacy := t.TempDir()
	makeLegacyDB(t, filepath.Join(legacy, "debrowser.db"))
	if err := os.MkdirAll(filepath.Join(legacy, "backups"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "backups", "b1.txt"), []byte("backup-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacy, "profiles", "p1"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "profiles", "p1", "prefs.txt"), []byte("prefs"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyHash, err := hashFile(filepath.Join(legacy, "debrowser.db"))
	if err != nil {
		t.Fatal(err)
	}

	layout := testLayout(t)
	report, err := Run(layout, legacy)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Needed || !report.DatabaseCopied {
		t.Errorf("report = %+v", report)
	}
	if report.FilesCopied != 2 {
		t.Errorf("files copied = %d, want 2", report.FilesCopied)
	}
	if got := readTheme(t, layout.DatabaseFile); got != "warm" {
		t.Errorf("migrated theme = %q, want warm", got)
	}
	// Legacy data must be completely intact.
	afterHash, err := hashFile(filepath.Join(legacy, "debrowser.db"))
	if err != nil {
		t.Fatal(err)
	}
	if afterHash != legacyHash {
		t.Error("legacy database was modified by migration")
	}
	if _, err := os.Stat(filepath.Join(layout.Data, "migration-report.json")); err != nil {
		t.Errorf("migration report not written: %v", err)
	}

	// Idempotent: a second run must not copy or overwrite anything.
	report2, err := Run(layout, legacy)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if report2.DatabaseCopied || report2.FilesCopied != 0 {
		t.Errorf("second run report = %+v, want no copies", report2)
	}
	if got := readTheme(t, layout.DatabaseFile); got != "warm" {
		t.Errorf("theme after second run = %q", got)
	}
}

func TestMigrateSkipsWhenTargetExists(t *testing.T) {
	legacy := t.TempDir()
	makeLegacyDB(t, filepath.Join(legacy, "debrowser.db"))
	layout := testLayout(t)
	makeLegacyDB(t, layout.DatabaseFile) // portable db already has its own data

	report, err := Run(layout, legacy)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.DatabaseCopied {
		t.Error("migration overwrote an existing portable database")
	}
}

func TestMigrateFailureKeepsLegacyAndCreatesNothing(t *testing.T) {
	legacy := t.TempDir()
	// A directory where the database file should be makes the copy fail.
	if err := os.MkdirAll(filepath.Join(legacy, "debrowser.db"), 0o750); err != nil {
		t.Fatal(err)
	}
	layout := testLayout(t)
	if _, err := Run(layout, legacy); err == nil {
		t.Fatal("Run succeeded despite an unreadable legacy database")
	}
	if _, err := os.Stat(layout.DatabaseFile); !os.IsNotExist(err) {
		t.Error("a partial target database was left behind after failure")
	}
	if info, err := os.Stat(filepath.Join(legacy, "debrowser.db")); err != nil || !info.IsDir() {
		t.Error("legacy data was damaged by the failed migration")
	}
}

func TestMigrateFreshInstall(t *testing.T) {
	layout := testLayout(t)
	report, err := Run(layout, filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Needed {
		t.Errorf("fresh install reported Needed: %+v", report)
	}
}

// The legacy (Stage 1) location and the portable target may both live
// under directories whose names contain URI-special characters —
// spaces, '#', '%' and non-ASCII text. The migration's read-only
// connections must still open exactly the intended files.
func TestMigrateLegacyDataUnderSpecialCharPaths(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "旧 数据#1%", "DeBrowser")
	if err := os.MkdirAll(legacyRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	makeLegacyDB(t, filepath.Join(legacyRoot, "debrowser.db"))

	layout, err := paths.Resolve(filepath.Join(base, "便携 目录#2%", "DeBrowser"))
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}

	report, err := Run(layout, legacyRoot)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.Needed || !report.DatabaseCopied {
		t.Fatalf("report = %+v, want needed + database copied", report)
	}
	if got := readTheme(t, layout.DatabaseFile); got != "warm" {
		t.Errorf("migrated theme = %q, want warm", got)
	}
	// The legacy database is left untouched.
	if got := readTheme(t, filepath.Join(legacyRoot, "debrowser.db")); got != "warm" {
		t.Errorf("legacy theme after migration = %q, want warm (untouched)", got)
	}
}
