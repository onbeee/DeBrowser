package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 9B review round 3: migration 0014 adds the
// instance_protection fallback journal. Existing instances and
// runtime records must survive untouched, and journal entries
// must cascade with their instance.

const instanceRuntimeV13DDL = `
CREATE TABLE instance_runtime (
    instance_id        TEXT PRIMARY KEY REFERENCES browser_instances(id) ON DELETE CASCADE,
    pid                INTEGER NOT NULL,
    process_created_at INTEGER NOT NULL,
    platform           TEXT NOT NULL DEFAULT '',
    executable_path    TEXT NOT NULL DEFAULT '',
    user_data_dir      TEXT NOT NULL DEFAULT '',
    started_at         TEXT NOT NULL DEFAULT ''
);`

func TestMigration14UpgradesV13PreservingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.db")
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
	for v := 1; v <= 13; v++ {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, 'historical')`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx, browserInstancesV12DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, instanceRuntimeV13DDL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO browser_instances (id, name, user_data_dir, status)
		 VALUES ('i1', '实例一', 'profiles/P-1', 'running')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO instance_runtime (instance_id, pid, process_created_at)
		 VALUES ('i1', 1234, 5678)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 14 {
		t.Fatalf("AppliedVersion = %d (err %v), want 14", v, err)
	}
	// The instance and its runtime record survived verbatim.
	var name, dir, status string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name, user_data_dir, status FROM browser_instances WHERE id = 'i1'`).Scan(&name, &dir, &status); err != nil {
		t.Fatalf("instance lost in upgrade: %v", err)
	}
	if name != "实例一" || dir != "profiles/P-1" || status != "running" {
		t.Fatalf("instance changed in upgrade: %q %q %q", name, dir, status)
	}
	var pid int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT pid FROM instance_runtime WHERE instance_id = 'i1'`).Scan(&pid); err != nil || pid != 1234 {
		t.Fatalf("runtime record changed in upgrade: pid %d (err %v)", pid, err)
	}
	// Journal entries work and cascade with their instance.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO instance_protection (instance_id, pid, process_created_at) VALUES ('i1', 4321, 8765)`); err != nil {
		t.Fatalf("insert protection: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM browser_instances WHERE id = 'i1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM instance_protection`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("protection rows after instance delete = %d (err %v), want 0 (cascade)", n, err)
	}
	// Idempotent.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigration14FreshDatabase(t *testing.T) {
	db := openTestDB(t)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 14 {
		t.Fatalf("AppliedVersion = %d (err %v), want 14", v, err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='instance_protection'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("instance_protection exists = %d (err %v), want 1", n, err)
	}
}
