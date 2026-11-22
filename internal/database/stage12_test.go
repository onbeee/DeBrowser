package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 9A (product adjustment): migration 0012 rebuilds
// external_browsers from the 0011 verified-registration shape
// into a pure path registration table. A database that already
// carries 0011-era registrations (with verification data) must
// upgrade without losing them: id, name, path and timestamps are
// preserved; the verification columns disappear.

const externalBrowsers0011DDL = `
CREATE TABLE external_browsers (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    browser_type        TEXT NOT NULL
                        CHECK (browser_type IN ('chrome', 'brave')),
    version             TEXT NOT NULL DEFAULT '',
    executable_path     TEXT NOT NULL,
    signature_status    TEXT NOT NULL DEFAULT '',
    signature_publisher TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'verified'
                        CHECK (status IN ('verified', 'invalid')),
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    last_verified_at    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_external_browsers_path
    ON external_browsers (executable_path);`

func TestMigration12PreservesRegistrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v11.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	// Recreate the v11 state: schema_migrations marks 1..11
	// applied, and external_browsers has its 0011 shape with one
	// verified registration in it.
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		t.Fatal(err)
	}
	for v := 1; v <= 11; v++ {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, 'historical')`, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx, externalBrowsers0011DDL); err != nil {
		t.Fatal(err)
	}
	// A real v11 database also carries browser_instances (created
	// by migration 0002); later migrations (0013) index it, so the
	// fixture includes its minimal shape.
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE browser_instances (
		id TEXT PRIMARY KEY, user_data_dir TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO external_browsers
		   (id, name, browser_type, version, executable_path, signature_status, signature_publisher, status, created_at, updated_at, last_verified_at)
		 VALUES ('b1', 'Google Chrome', 'chrome', '142.0.7444.162', 'C:\chrome.exe', 'verified', 'Google LLC', 'verified', '2026-11-15 10:00:00', '2026-11-15 10:00:00', '2026-11-15T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Upgrade.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 16 {
		t.Fatalf("AppliedVersion = %d (err %v), want 12", v, err)
	}
	// The registration survived as a pure path record.
	var id, name, exePath, created string
	err = db.SQL().QueryRowContext(ctx,
		`SELECT id, name, executable_path, created_at FROM external_browsers`).Scan(&id, &name, &exePath, &created)
	if err != nil {
		t.Fatalf("registration lost in upgrade: %v", err)
	}
	if id != "b1" || name != "Google Chrome" || exePath != `C:\chrome.exe` || created != "2026-11-15 10:00:00" {
		t.Fatalf("registration changed: %q %q %q %q", id, name, exePath, created)
	}
	// Verification columns are gone, the unique path rule remains.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO external_browsers (id, name, executable_path) VALUES ('b2', 'dupe', 'C:\chrome.exe')`); err == nil {
		t.Fatal("duplicate executable_path must still fail after upgrade")
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
}
