package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 9A: migration 0010 adds the kernels table for
// DeBrowser-managed Chromium builds only.

func TestMigration10FreshDatabase(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 10 {
		t.Fatalf("AppliedVersion = %d (err %v), want 10", v, err)
	}
	var name string
	err = db.SQL().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='kernels'`).Scan(&name)
	if err != nil || name != "kernels" {
		t.Fatalf("kernels table missing: %v", err)
	}
	// The (version, platform) uniqueness must exist.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO kernels (id, version, install_path, executable_path) VALUES ('k1', '142.0.1.2', 'kernels/142.0.1.2', 'kernels/142.0.1.2/chrome.exe')`); err != nil {
		t.Fatalf("insert kernel: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO kernels (id, version, install_path, executable_path) VALUES ('k2', '142.0.1.2', 'kernels/142.0.1.2', 'kernels/142.0.1.2/chrome.exe')`); err == nil {
		t.Fatal("duplicate (version, platform) insert must fail")
	}
	// Status CHECK must reject values outside the vocabulary.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO kernels (id, version, install_path, executable_path, status) VALUES ('k3', '1.0', 'p', 'e', 'bogus')`); err == nil {
		t.Fatal("invalid kernel status must fail the CHECK")
	}
}

func TestMigration10UpgradesPopulatedV9KeepingData(t *testing.T) {
	// 0010 is a pure CREATE TABLE with no data movement: seed
	// historical rows beside the new table and assert coexistence
	// and integrity. (The step-by-step populated upgrade chain for
	// 0001–0009 is covered by the earlier stage tests.)
	db, err := Open(filepath.Join(t.TempDir(), "populated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, address, config_json, secret_ref, source)
		 VALUES ('p1', 'hist', 'socks5', '127.0.0.1', '{}', 'ref-hist', 'custom')`); err != nil {
		t.Fatalf("seed historical proxy: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO browser_instances (id, name, user_data_dir, proxy_id) VALUES ('i1', 'inst', 'data/profiles/i1', 'p1')`); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO kernels (id, version, install_path, executable_path) VALUES ('k1', '142.0.1.2', 'kernels/142.0.1.2', 'kernels/142.0.1.2/chrome.exe')`); err != nil {
		t.Fatalf("insert kernel beside historical data: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM proxies`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("historical proxies = %d, %v", n, err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM kernels`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("kernels = %d, %v", n, err)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_key_check`).Scan(&n); err == nil {
		t.Fatal("foreign_key_check should return no rows")
	}
}
