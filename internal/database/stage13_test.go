package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 9B: migration 0013 adds the instance_runtime process
// identity table and the profile_counter sequence. Existing
// instances, registrations and bookmarks must survive untouched,
// and the runtime table must cascade with its instance.

const browserInstancesV12DDL = `
CREATE TABLE browser_instances (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    kernel_id       TEXT NOT NULL DEFAULT '',
    kernel_name     TEXT NOT NULL DEFAULT '',
    kernel_version  TEXT NOT NULL DEFAULT '',
    user_data_dir   TEXT NOT NULL UNIQUE,
    proxy_id        TEXT,
    proxy_listen_port INTEGER,
    status          TEXT NOT NULL DEFAULT 'stopped'
                    CHECK (status IN ('stopped', 'running', 'error')),
    start_url       TEXT NOT NULL DEFAULT '',
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);`

func TestMigration13UpgradesV12PreservingInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v12.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 12; v++ {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, 'historical')`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx, browserInstancesV12DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO browser_instances (id, name, user_data_dir, status)
		 VALUES ('i1', '实例一', 'profiles/P-1', 'running')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 14 {
		t.Fatalf("AppliedVersion = %d (err %v), want 13", v, err)
	}
	// The instance survived verbatim.
	var name, dir, status string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name, user_data_dir, status FROM browser_instances WHERE id = 'i1'`).Scan(&name, &dir, &status); err != nil {
		t.Fatalf("instance lost in upgrade: %v", err)
	}
	if name != "实例一" || dir != "profiles/P-1" || status != "running" {
		t.Fatalf("instance changed in upgrade: %q %q %q", name, dir, status)
	}
	// The counter starts at 1.
	var seq int
	if err := db.SQL().QueryRowContext(ctx, `SELECT next_seq FROM profile_counter WHERE id = 1`).Scan(&seq); err != nil || seq != 1 {
		t.Fatalf("profile_counter next_seq = %d (err %v), want 1", seq, err)
	}
	// Runtime records work and cascade with their instance.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO instance_runtime (instance_id, pid, process_created_at) VALUES ('i1', 1234, 5678)`); err != nil {
		t.Fatalf("insert runtime: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM browser_instances WHERE id = 'i1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM instance_runtime`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("runtime rows after instance delete = %d (err %v), want 0 (cascade)", n, err)
	}
	// Idempotent.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigration13FreshDatabase(t *testing.T) {
	db := openTestDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 14 {
		t.Fatalf("AppliedVersion = %d (err %v), want 13", v, err)
	}
	for _, table := range []string{"instance_runtime", "profile_counter"} {
		var n int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing on a fresh database", table)
		}
	}
}
