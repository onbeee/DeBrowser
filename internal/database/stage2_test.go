package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/paths"
)

// openV1Database builds a database migrated only to version 1 (the
// Stage 1 state) holding one recognizable settings row.
func openV1Database(t *testing.T, path string, opts Options) *DB {
	t.Helper()
	db, err := OpenWithOptions(path, opts)
	if err != nil {
		t.Fatalf("OpenWithOptions() error = %v", err)
	}
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatalf("create bookkeeping: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var v1 []migration
	for _, m := range migs {
		if m.version == 1 {
			v1 = append(v1, m)
		}
	}
	if err := db.applyMigrations(ctx, v1); err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO app_settings (key, value) VALUES ('marker', 'stage1-data')`); err != nil {
		t.Fatalf("insert marker: %v", err)
	}
	return db
}

func appliedVersions(t *testing.T, db *DB) []int {
	t.Helper()
	rows, err := db.SQL().QueryContext(context.Background(),
		`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query versions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestMigrateFromEmptyAppliesAllInOrder(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	got := appliedVersions(t, db)
	if len(got) != len(migs) {
		t.Fatalf("applied = %v, want %d versions", got, len(migs))
	}
	for i, m := range migs {
		if got[i] != m.version {
			t.Fatalf("applied order = %v, migrations order mismatch at %d", got, i)
		}
	}
	// Core tables from the Stage 2 migrations exist.
	for _, table := range []string{"proxies", "browser_instances", "bookmarks", "app_settings"} {
		var n int
		err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s exists = %d (err %v), want 1", table, n, err)
		}
	}
	v, err := db.AppliedVersion(ctx)
	if err != nil || v != migs[len(migs)-1].version {
		t.Errorf("AppliedVersion = %d (err %v), want %d", v, err, migs[len(migs)-1].version)
	}
}

func TestUpgradeFromV1PreservesDataAndRecordsOrder(t *testing.T) {
	dir := t.TempDir()
	db := openV1Database(t, filepath.Join(dir, "test.db"), Options{})
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (upgrade): %v", err)
	}
	got := appliedVersions(t, db)
	want := []int{1, 2, 3, 4, 5, 6}
	if len(got) != len(want) {
		t.Fatalf("applied = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("applied = %v, want %v", got, want)
		}
	}
	var marker string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='marker'`).Scan(&marker); err != nil {
		t.Fatalf("marker read: %v", err)
	}
	if marker != "stage1-data" {
		t.Errorf("marker = %q, want stage1-data (upgrade must preserve data)", marker)
	}
	// Second run is a no-op (idempotent upgrade).
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := appliedVersions(t, db); len(got) != 6 {
		t.Errorf("after second Migrate applied = %v, want 6 rows", got)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	bad := []migration{
		{version: 901, name: "0901_good.sql", sql: `CREATE TABLE t_good (id INTEGER);`},
		{version: 902, name: "0902_bad.sql", sql: `CREATE TABLE t_bad (id INTEGER); THIS IS NOT SQL;`},
	}
	if err := db.applyMigrations(ctx, bad); err == nil {
		t.Fatal("applyMigrations with a broken migration succeeded, want error")
	}
	// The good migration committed; the bad one left nothing behind:
	// no table and no bookkeeping row.
	var badTable, badRow int
	_ = db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='t_bad'`).Scan(&badTable)
	_ = db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM schema_migrations WHERE version=902`).Scan(&badRow)
	if badTable != 0 || badRow != 0 {
		t.Errorf("failed migration left traces: table=%d row=%d, want 0/0", badTable, badRow)
	}
	var goodRow int
	_ = db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM schema_migrations WHERE version=901`).Scan(&goodRow)
	if goodRow != 1 {
		t.Errorf("good migration row = %d, want 1", goodRow)
	}
}

func TestPreMigrationBackupCapturesWALData(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backups, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "test.db")
	db := openV1Database(t, path, Options{BackupDir: backups})
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	entries, err := os.ReadDir(backups)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup files = %v (err %v), want exactly 1", entries, err)
	}
	name := entries[0].Name()
	if !strings.HasPrefix(name, "debrowser-pre-migration-") || !strings.Contains(name, "from-v1") {
		t.Errorf("backup name = %q, want debrowser-pre-migration-...-from-v1-to-v3.db", name)
	}
	// The backup is a complete, openable database holding the v1 data
	// (which lived in the WAL when the backup was taken).
	bak, err := Open(filepath.Join(backups, name))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer func() { _ = bak.Close() }()
	var marker string
	if err := bak.SQL().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='marker'`).Scan(&marker); err != nil {
		t.Fatalf("read marker from backup: %v", err)
	}
	if marker != "stage1-data" {
		t.Errorf("backup marker = %q, want stage1-data", marker)
	}
	if v, err := bak.AppliedVersion(ctx); err != nil || v != 1 {
		t.Errorf("backup version = %d (err %v), want 1 (pre-migration state)", v, err)
	}
	_ = db.Close()
}

func TestBackupFailureAbortsMigration(t *testing.T) {
	dir := t.TempDir()
	// BackupDir pointing at a regular file makes the backup fail.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := openV1Database(t, filepath.Join(dir, "test.db"), Options{BackupDir: blocker})
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err == nil {
		t.Fatal("Migrate succeeded with an impossible backup, want error")
	}
	// No migration was applied: the schema is still v1.
	if v, err := db.AppliedVersion(ctx); err != nil || v != 1 {
		t.Errorf("AppliedVersion = %d (err %v), want 1 after aborted migration", v, err)
	}
	var proxies int
	_ = db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='proxies'`).Scan(&proxies)
	if proxies != 0 {
		t.Error("proxies table exists even though the migration was aborted")
	}
}

func TestSynchronousFullAndPoolBehavior(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	var sync int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
		t.Fatalf("PRAGMA synchronous: %v", err)
	}
	if sync != 2 {
		t.Errorf("synchronous = %d, want 2 (FULL)", sync)
	}
}

func TestWithTxCommitAndRollback(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	insert := func(tx *sql.Tx, key string) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO app_settings (key, value) VALUES (?, 'v')`, key)
		return err
	}
	if err := db.WithTx(ctx, func(tx *sql.Tx) error { return insert(tx, "committed") }); err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	sentinel := fmt.Errorf("boom")
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		if err := insert(tx, "rolled-back"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want the fn error", err)
	}
	var committed, rolled int
	_ = db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM app_settings WHERE key='committed'`).Scan(&committed)
	_ = db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM app_settings WHERE key='rolled-back'`).Scan(&rolled)
	if committed != 1 || rolled != 0 {
		t.Errorf("committed=%d rolled-back=%d, want 1/0", committed, rolled)
	}
}

func TestCloseAndReopenPreservesData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	ctx := context.Background()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO app_settings (key, value) VALUES ('k', 'persisted')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = db2.Close() }()
	var v string
	if err := db2.SQL().QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key='k'`).Scan(&v); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if v != "persisted" {
		t.Errorf("value after reopen = %q, want persisted", v)
	}
}

func TestDatabaseLivesInPortableLayout(t *testing.T) {
	layout, err := paths.Resolve(filepath.Join(t.TempDir(), "DeBrowser"))
	if err != nil {
		t.Fatalf("paths.Resolve: %v", err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatalf("layout.Ensure: %v", err)
	}
	db, err := OpenWithOptions(layout.DatabaseFile, Options{BackupDir: layout.Backups})
	if err != nil {
		t.Fatalf("Open in layout: %v", err)
	}
	defer func() { _ = db.Close() }()
	if db.Path() != layout.DatabaseFile {
		t.Errorf("db path = %q, want layout path %q", db.Path(), layout.DatabaseFile)
	}
	if !strings.HasPrefix(db.Path(), layout.Root) {
		t.Errorf("db path %q escapes the portable root %q", db.Path(), layout.Root)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate in layout: %v", err)
	}
	if _, err := os.Stat(layout.DatabaseFile); err != nil {
		t.Errorf("database file missing at layout path: %v", err)
	}
}

func TestOpenInUnwritableLocationFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(blocker, "test.db"))
	if err == nil {
		_ = db.Close()
		t.Fatal("Open under a regular file succeeded, want an explicit error (no fallback)")
	}
}
