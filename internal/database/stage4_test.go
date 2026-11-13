package database

import (
	"context"
	"path/filepath"
	"testing"
)

// openV3Database builds a database migrated only to version 3 (the
// Stage 3 state) holding recognizable proxy, instance and bookmark
// rows written with the pre-0004 column sets.
func openV3Database(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
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
	var upto3 []migration
	for _, m := range migs {
		if m.version <= 3 {
			upto3 = append(upto3, m)
		}
	}
	if err := db.applyMigrations(ctx, upto3); err != nil {
		t.Fatalf("apply up to v3: %v", err)
	}
	stmts := []string{
		`INSERT INTO proxies (id, name, protocol, address, port, secret_ref)
		 VALUES ('p1', '旧节点', 'socks5', '127.0.0.1', 1080, 'ref-old')`,
		`INSERT INTO browser_instances (id, name, user_data_dir, proxy_id)
		 VALUES ('i1', '旧实例', 'profiles/i1', 'p1')`,
		`INSERT INTO bookmarks (id, name, url) VALUES ('b1', '旧书签', 'https://example.com')`,
	}
	for _, q := range stmts {
		if _, err := db.SQL().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed v3 data: %v", err)
		}
	}
	return db
}

// TestUpgradeFromV3ToV4PreservesData: the 0004 migration must apply
// cleanly over a populated Stage 3 database, lose nothing, and give
// existing rows the designed defaults.
func TestUpgradeFromV3ToV4PreservesData(t *testing.T) {
	dir := t.TempDir()
	db := openV3Database(t, filepath.Join(dir, "test.db"))
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (v3 -> v4): %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 4 {
		t.Fatalf("AppliedVersion = %d (err %v), want 4", v, err)
	}
	// New tables exist.
	for _, table := range []string{"proxy_groups", "proxy_tags", "subscriptions"} {
		var n int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s exists = %d (err %v), want 1", table, n, err)
		}
	}
	// Old proxy row intact, with the new columns defaulted.
	var name, source, dedup string
	var groupID, subID, listenPort any
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name, source, dedup_key, group_id, subscription_id FROM proxies WHERE id='p1'`).
		Scan(&name, &source, &dedup, &groupID, &subID); err != nil {
		t.Fatalf("read upgraded proxy: %v", err)
	}
	if name != "旧节点" || source != "manual" || dedup != "" || groupID != nil || subID != nil {
		t.Errorf("upgraded proxy = (%q, %q, %q, %v, %v), want （旧节点， manual, '', nil, nil)",
			name, source, dedup, groupID, subID)
	}
	// Old instance and bookmark intact; listen port defaults to NULL.
	var instName string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name, proxy_listen_port FROM browser_instances WHERE id='i1'`).Scan(&instName, &listenPort); err != nil {
		t.Fatalf("read upgraded instance: %v", err)
	}
	if instName != "旧实例" || listenPort != nil {
		t.Errorf("upgraded instance = (%q, %v), want （旧实例， nil)", instName, listenPort)
	}
	var bm string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name FROM bookmarks WHERE id='b1'`).Scan(&bm); err != nil || bm != "旧书签" {
		t.Errorf("bookmark = %q (err %v), want 旧书签", bm, err)
	}
	// Idempotent: a second run changes nothing.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := appliedVersions(t, db); len(got) != 4 {
		t.Errorf("after second Migrate applied = %v, want 4 rows", got)
	}
}
