package database

import (
	"context"
	"path/filepath"
	"testing"
)

// openV4Database builds a database migrated only to version 4 (the
// Stage 4A state) populated with group, tag, subscription, proxy and
// instance rows written with the full post-0004 column sets — the
// exact data shape migration 0005 must carry over while removing the
// group/tag structures.
func openV4Database(t *testing.T, path string) *DB {
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
	var upto4 []migration
	for _, m := range migs {
		if m.version <= 4 {
			upto4 = append(upto4, m)
		}
	}
	if err := db.applyMigrations(ctx, upto4); err != nil {
		t.Fatalf("apply up to v4: %v", err)
	}
	stmts := []string{
		`INSERT INTO proxy_groups (id, name, sort_order) VALUES ('g1', '分组一', 1)`,
		`INSERT INTO subscriptions (id, name, url_hint, secret_ref, group_id, allow_lan,
			auto_update_minutes, last_updated_at, last_status, last_error)
		 VALUES ('s1', '订阅一', 'https://example.com', 'sub-ref', 'g1', 0, 0,
			'2026-01-01 00:00:00', 'ok', '')`,
		`INSERT INTO proxies (id, name, protocol, address, port, config_json, secret_ref,
			enabled, group_id, sort_order, source, subscription_id, dedup_key,
			last_latency_ms, last_test_ok, last_tested_at)
		 VALUES ('p1', '订阅节点', 'vless', 'example.com', 443, '{}', 'ref-p1',
			1, 'g1', 7, 'subscription', 's1', 'dk-p1', 42, 1, '2026-01-01 00:00:01')`,
		`INSERT INTO proxies (id, name, protocol, address, port, secret_ref, sort_order)
		 VALUES ('p2', '手动节点', 'socks5', '127.0.0.1', 1080, 'ref-p2', 3)`,
		`INSERT INTO proxy_tags (proxy_id, tag) VALUES ('p1', '香港'), ('p1', '高速'), ('p2', '备用')`,
		`INSERT INTO browser_instances (id, name, user_data_dir, proxy_id, proxy_listen_port)
		 VALUES ('i1', '实例一', 'profiles/i1', 'p1', 19001)`,
		`INSERT INTO bookmarks (id, name, url) VALUES ('b1', '书签一', 'https://example.com')`,
	}
	for _, q := range stmts {
		if _, err := db.SQL().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed v4 data: %v", err)
		}
	}
	return db
}

// TestUpgradeFromV4ToV5RemovesGroupsAndTags: migration 0005 rebuilds
// proxies and subscriptions without group_id and drops the group/tag
// tables. Every other row, value and relationship — including the
// instance -> proxy binding and the subscription -> proxy cascade —
// must survive, which is also the proof that the deferred FK handling
// in 0005 does not fire SET NULL / CASCADE actions during the swap.
func TestUpgradeFromV4ToV5RemovesGroupsAndTags(t *testing.T) {
	dir := t.TempDir()
	db := openV4Database(t, filepath.Join(dir, "test.db"))
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (v4 -> v5): %v", err)
	}
	// Migrate runs the whole chain; since Stage 6 the latest
	// version is 6. The 0005-specific assertions below (removed
	// tables, preserved rows) are unaffected by 0006.
	if v, err := db.AppliedVersion(ctx); err != nil || v != 9 {
		t.Fatalf("AppliedVersion = %d (err %v), want 6", v, err)
	}

	// Removed structures are gone; no rebuild leftovers remain.
	for _, table := range []string{"proxy_groups", "proxy_tags", "proxies_new", "subscriptions_new"} {
		var n int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 0 {
			t.Errorf("table %s exists = %d (err %v), want 0", table, n, err)
		}
	}
	for _, tc := range []struct{ table, column string }{
		{"proxies", "group_id"}, {"subscriptions", "group_id"},
	} {
		cols := tableColumns(t, db, tc.table)
		if cols[tc.column] {
			t.Errorf("%s.%s still present after 0005", tc.table, tc.column)
		}
	}
	if !tableColumns(t, db, "proxies")["sort_order"] {
		t.Errorf("proxies.sort_order missing after 0005, want preserved")
	}
	// Index state: group index gone, dedup unique index present.
	var idxN int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='index' AND name='idx_proxies_group'`).Scan(&idxN); err != nil || idxN != 0 {
		t.Errorf("idx_proxies_group exists = %d (err %v), want 0", idxN, err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='index' AND name='idx_proxies_dedup'`).Scan(&idxN); err != nil || idxN != 1 {
		t.Errorf("idx_proxies_dedup exists = %d (err %v), want 1", idxN, err)
	}

	// Proxy rows fully preserved.
	var name, source, dedup, secretRef string
	var sortOrder, latency int
	var subID any
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name, source, subscription_id, dedup_key, sort_order, secret_ref, last_latency_ms
		 FROM proxies WHERE id='p1'`).Scan(&name, &source, &subID, &dedup, &sortOrder, &secretRef, &latency); err != nil {
		t.Fatalf("read upgraded p1: %v", err)
	}
	if name != "订阅节点" || source != "subscription" || subID != "s1" || dedup != "dk-p1" ||
		sortOrder != 7 || secretRef != "ref-p1" || latency != 42 {
		t.Errorf("p1 = (%q, %q, %v, %q, %d, %q, %d), want （订阅节点， subscription, s1, dk-p1, 7, ref-p1, 42)",
			name, source, subID, dedup, sortOrder, secretRef, latency)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT sort_order FROM proxies WHERE id='p2'`).Scan(&sortOrder); err != nil || sortOrder != 3 {
		t.Errorf("p2 sort_order = %d (err %v), want 3", sortOrder, err)
	}

	// Subscription row preserved.
	var subName, subRef string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT name, secret_ref FROM subscriptions WHERE id='s1'`).Scan(&subName, &subRef); err != nil {
		t.Fatalf("read upgraded s1: %v", err)
	}
	if subName != "订阅一" || subRef != "sub-ref" {
		t.Errorf("s1 = (%q, %q), want （订阅一， sub-ref)", subName, subRef)
	}

	// Instance binding and listen port preserved — the deferred-FK
	// proof: a SET NULL fired during the rebuild would show up here.
	var instProxy any
	var listenPort int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT proxy_id, proxy_listen_port FROM browser_instances WHERE id='i1'`).Scan(&instProxy, &listenPort); err != nil {
		t.Fatalf("read upgraded i1: %v", err)
	}
	if instProxy != "p1" || listenPort != 19001 {
		t.Errorf("i1 = (proxy %v, port %d), want (p1, 19001)", instProxy, listenPort)
	}

	// Whole-database integrity after the rebuild.
	assertFKAndIntegrityOK(t, db)

	// dedup_key uniqueness still enforced on the rebuilt table.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, dedup_key) VALUES ('p3', '重复节点', 'http', 'dk-p1')`); err == nil {
		t.Errorf("duplicate dedup_key insert succeeded after 0005, want constraint failure")
	}

	// FK actions still behave on the rebuilt tables: deleting the
	// subscription cascades to its proxies, which in turn unbinds the
	// instance (SET NULL) instead of deleting it.
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM subscriptions WHERE id='s1'`); err != nil {
		t.Fatalf("delete s1: %v", err)
	}
	var p1Count int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM proxies WHERE id='p1'`).Scan(&p1Count); err != nil || p1Count != 0 {
		t.Errorf("p1 after subscription delete = %d (err %v), want 0 (CASCADE)", p1Count, err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT proxy_id FROM browser_instances WHERE id='i1'`).Scan(&instProxy); err != nil {
		t.Fatalf("read i1 after cascade: %v", err)
	}
	if instProxy != nil {
		t.Errorf("i1.proxy_id after cascade = %v, want nil (SET NULL)", instProxy)
	}
	assertFKAndIntegrityOK(t, db)

	// Idempotent: a second run changes nothing.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if got := appliedVersions(t, db); len(got) != 9 {
		t.Errorf("after second Migrate applied = %v, want 6 rows", got)
	}
}

func tableColumns(t *testing.T, db *DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.SQL().QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s) rows: %v", table, err)
	}
	return cols
}

func assertFKAndIntegrityOK(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	fkRows, err := db.SQL().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = fkRows.Close() }()
	if fkRows.Next() {
		t.Errorf("foreign_key_check returned a violation row, want none")
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q, want ok", integrity)
	}
}
