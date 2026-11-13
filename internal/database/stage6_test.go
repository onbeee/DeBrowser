package database

import (
	"context"
	"path/filepath"
	"testing"
)

// openV5Database builds a database migrated only to version 5 (the
// post-Stage-5B state) populated with the Stage 4A/5 data shapes
// migration 0006 must carry over untouched.
func openV5Database(t *testing.T, path string) *DB {
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
	var upto5 []migration
	for _, m := range migs {
		if m.version <= 5 {
			upto5 = append(upto5, m)
		}
	}
	if err := db.applyMigrations(ctx, upto5); err != nil {
		t.Fatalf("apply up to v5: %v", err)
	}
	stmts := []string{
		`INSERT INTO subscriptions (id, name, url_hint, secret_ref, allow_lan,
			auto_update_minutes, last_updated_at, last_status, last_error)
		 VALUES ('s1', '订阅一', 'https://example.com', 'sub-ref-1', 0, 0,
			'2026-01-01 00:00:00', 'ok', '')`,
		`INSERT INTO proxies (id, name, protocol, address, port, config_json, secret_ref,
			enabled, sort_order, source, subscription_id, dedup_key,
			last_latency_ms, last_test_ok, last_tested_at)
		 VALUES ('p1', '订阅节点', 'vless', 'example.com', 443, '{}', 'ref-p1',
			1, 7, 'subscription', 's1', 'dk-p1', 42, 1, '2026-01-01 00:00:01')`,
		`INSERT INTO proxies (id, name, protocol, address, port, secret_ref, sort_order)
		 VALUES ('p2', '手动节点', 'socks5', '127.0.0.1', 1080, 'ref-p2', 3)`,
		`INSERT INTO browser_instances (id, name, user_data_dir, proxy_id, proxy_listen_port)
		 VALUES ('i1', '实例一', 'profiles/i1', 'p1', 19001)`,
		`INSERT INTO bookmarks (id, name, url) VALUES ('b1', '书签一', 'https://example.com')`,
	}
	for _, q := range stmts {
		if _, err := db.SQL().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed v5 data: %v", err)
		}
	}
	return db
}

// TestUpgradeFromV5ToV6PreservesData: migration 0006 adds the
// Mihomo config tables and the proxies.config_id / source_node_name
// columns without touching any existing row, value or relation.
func TestUpgradeFromV5ToV6PreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db := openV5Database(t, path)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if v, err := db.AppliedVersion(ctx); err != nil || v != 5 {
		t.Fatalf("pre-upgrade AppliedVersion = %d (err %v), want 5", v, err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (v5 -> v6): %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 6 {
		t.Fatalf("AppliedVersion = %d (err %v), want 6", v, err)
	}

	// New structures exist.
	for _, table := range []string{"mihomo_configs", "mihomo_proxy_groups", "mihomo_proxy_group_members"} {
		var name string
		err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing after 0006: %v", table, err)
		}
	}
	cols := tableColumns(t, db, "proxies")
	for _, col := range []string{"config_id", "source_node_name"} {
		if !cols[col] {
			t.Fatalf("proxies.%s missing after 0006", col)
		}
	}

	// Old rows are byte-identical in every pre-existing field, and
	// the new columns carry their compatibility defaults.
	var (
		name, protocol, secretRef, source, dedup string
		subID                                    string
		configID                                 any
		sourceNodeName                           string
	)
	err := db.SQL().QueryRowContext(ctx, `SELECT name, protocol, secret_ref, source,
		subscription_id, dedup_key, config_id, source_node_name FROM proxies WHERE id='p1'`).
		Scan(&name, &protocol, &secretRef, &source, &subID, &dedup, &configID, &sourceNodeName)
	if err != nil {
		t.Fatalf("read p1: %v", err)
	}
	if name != "订阅节点" || protocol != "vless" || secretRef != "ref-p1" ||
		source != "subscription" || subID != "s1" || dedup != "dk-p1" {
		t.Fatalf("p1 changed by 0006: %q %q %q %q %q %q", name, protocol, secretRef, source, subID, dedup)
	}
	if configID != nil {
		t.Fatalf("p1.config_id = %v, want NULL default", configID)
	}
	if sourceNodeName != "" {
		t.Fatalf("p1.source_node_name = %q, want '' default", sourceNodeName)
	}
	var subRef string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT secret_ref FROM subscriptions WHERE id='s1'`).Scan(&subRef); err != nil || subRef != "sub-ref-1" {
		t.Fatalf("subscription vault ref changed: %q (err %v)", subRef, err)
	}
	var instanceProxy string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT proxy_id FROM browser_instances WHERE id='i1'`).Scan(&instanceProxy); err != nil || instanceProxy != "p1" {
		t.Fatalf("instance->proxy binding changed: %q (err %v)", instanceProxy, err)
	}
	var bookmarks int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM bookmarks`).Scan(&bookmarks); err != nil || bookmarks != 1 {
		t.Fatalf("bookmarks = %d (err %v), want 1", bookmarks, err)
	}

	assertFKAndIntegrity(t, db)
}

// TestFreshDatabaseReachesV6: an empty database migrates 0 -> 6
// with the Stage 6 tables present and checks clean.
func TestFreshDatabaseReachesV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 6 {
		t.Fatalf("AppliedVersion = %d (err %v), want 6", v, err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name LIKE 'mihomo%'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("mihomo tables = %d (err %v), want 3", n, err)
	}
	assertFKAndIntegrity(t, db)
}

// TestMihomoMemberCheckConstraints: the member table rejects rows
// whose target columns contradict member_kind.
func TestMihomoMemberCheckConstraints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checks.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seed := []string{
		`INSERT INTO mihomo_configs (id, name, source_type, file_path, sha256)
		 VALUES ('c1', '配置一', 'file', 'configs/c1.yaml', 'abc')`,
		`INSERT INTO mihomo_proxy_groups (id, config_id, name, type) VALUES ('g1', 'c1', '组一', 'select')`,
		`INSERT INTO proxies (id, name, protocol) VALUES ('px', '节点', 'socks5')`,
	}
	for _, q := range seed {
		if _, err := db.SQL().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	bad := []string{
		// proxy kind without proxy_id
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_name, member_kind)
		 VALUES ('g1', 0, 'x', 'proxy')`,
		// proxy kind with both targets
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_name, member_kind, proxy_id, target_group_id)
		 VALUES ('g1', 1, 'x', 'proxy', 'px', 'g1')`,
		// builtin kind with a target
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_name, member_kind, proxy_id)
		 VALUES ('g1', 2, 'DIRECT', 'builtin', 'px')`,
		// unknown kind
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_name, member_kind)
		 VALUES ('g1', 3, 'x', 'other')`,
	}
	for _, q := range bad {
		if _, err := db.SQL().ExecContext(ctx, q); err == nil {
			t.Fatalf("constraint not enforced: %s", q)
		}
	}
	good := `INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_name, member_kind, proxy_id)
		VALUES ('g1', 0, '节点', 'proxy', 'px')`
	if _, err := db.SQL().ExecContext(ctx, good); err != nil {
		t.Fatalf("valid member rejected: %v", err)
	}
	assertFKAndIntegrity(t, db)
}

func assertFKAndIntegrity(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.SQL().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		t.Fatal("foreign_key_check returned a violation row")
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
}
