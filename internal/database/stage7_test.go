package database

import (
	"context"
	"path/filepath"
	"testing"
)

// openV6Database builds a database migrated only to version 6 (the
// post-Stage-6 state) populated with every Stage 4A/6 data shape
// migration 0007 must carry over: a manual node bound to an
// instance, a subscription with its config, native groups and
// members, a file-imported node with its config and group, and
// vault references throughout.
func openV6Database(t *testing.T, path string) *DB {
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
	var upto6 []migration
	for _, m := range migs {
		if m.version <= 6 {
			upto6 = append(upto6, m)
		}
	}
	if err := db.applyMigrations(ctx, upto6); err != nil {
		t.Fatalf("apply up to v6: %v", err)
	}
	stmts := []string{
		`INSERT INTO subscriptions (id, name, url_hint, secret_ref, allow_lan,
			auto_update_minutes, last_updated_at, last_status, last_error)
		 VALUES ('s1', '订阅一', 'https://example.com', 'sub-ref-1', 0, 0,
			'2026-01-01 00:00:00', 'ok', '')`,
		`INSERT INTO mihomo_configs (id, name, source_type, subscription_id, file_path,
			sha256, secret_ref, format_version, security_report_json)
		 VALUES ('c1', '订阅一', 'subscription', 's1', 'configs/c1.yaml', 'aa', 'bundle-c1', 1, '[]')`,
		`INSERT INTO mihomo_configs (id, name, source_type, subscription_id, file_path,
			sha256, secret_ref, format_version, security_report_json)
		 VALUES ('c2', '本地配置', 'file', NULL, 'configs/c2.yaml', 'bb', '', 1, '[]')`,
		`INSERT INTO proxies (id, name, protocol, address, port, config_json, secret_ref,
			enabled, sort_order, source, subscription_id, dedup_key,
			last_latency_ms, last_test_ok, last_tested_at, config_id, source_node_name)
		 VALUES ('p1', '订阅节点', 'vless', 'a.example.com', 443, '{}', 'ref-p1',
			1, 7, 'subscription', 's1', 'dk-p1', 42, 1, '2026-01-01 00:00:01', 'c1', '订阅节点')`,
		`INSERT INTO proxies (id, name, protocol, address, port, secret_ref, sort_order, source)
		 VALUES ('p2', '手动节点', 'socks5', '127.0.0.1', 1080, 'ref-p2', 3, 'manual')`,
		`INSERT INTO proxies (id, name, protocol, address, port, config_json, secret_ref,
			enabled, sort_order, source, subscription_id, dedup_key, config_id, source_node_name)
		 VALUES ('p3', '文件节点', 'trojan', 'b.example.com', 443, '{}', 'ref-p3',
			1, 1, 'file_import', NULL, 'dk-p3', 'c2', '文件节点')`,
		`INSERT INTO mihomo_proxy_groups (id, config_id, name, type, options_json, sort_order)
		 VALUES ('g1', 'c1', '选择', 'select', '{}', 0)`,
		`INSERT INTO mihomo_proxy_groups (id, config_id, name, type, options_json, sort_order)
		 VALUES ('g2', 'c2', '自动', 'url-test', '{}', 0)`,
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_kind, proxy_id, target_group_id, member_name)
		 VALUES ('g1', 0, 'proxy', 'p1', NULL, '订阅节点')`,
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_kind, proxy_id, target_group_id, member_name)
		 VALUES ('g1', 1, 'builtin', NULL, NULL, 'DIRECT')`,
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_kind, proxy_id, target_group_id, member_name)
		 VALUES ('g2', 0, 'proxy', 'p3', NULL, '文件节点')`,
		`INSERT INTO mihomo_proxy_group_members (group_id, sort_order, member_kind, proxy_id, target_group_id, member_name)
		 VALUES ('g2', 1, 'group', NULL, 'g1', '选择')`,
		`INSERT INTO browser_instances (id, name, user_data_dir, proxy_id, proxy_listen_port)
		 VALUES ('i1', '实例一', 'profiles/i1', 'p2', 19001)`,
		`INSERT INTO bookmarks (id, name, url) VALUES ('b1', '书签一', 'https://example.com')`,
	}
	for _, q := range stmts {
		if _, err := db.SQL().ExecContext(ctx, q); err != nil {
			t.Fatalf("seed v6 data: %v", err)
		}
	}
	return db
}

// applyUptoVersion applies only the migrations up to version n,
// so a stage test can pin its own upgrade step even after later
// migrations land.
func applyUptoVersion(t *testing.T, db *DB, n int) {
	t.Helper()
	migs, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	var upto []migration
	for _, m := range migs {
		if m.version <= n {
			upto = append(upto, m)
		}
	}
	if err := db.applyMigrations(context.Background(), upto); err != nil {
		t.Fatalf("apply up to v%d: %v", n, err)
	}
}

// TestUpgradeFromV6ToV7SourceVocabulary: migration 0007 renames
// the proxy source vocabulary (manual -> custom,
// file_import -> file) while preserving every row, ID, vault
// reference, config/subscription link, instance binding and
// native group member relation exactly.
func TestUpgradeFromV6ToV7SourceVocabulary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db := openV6Database(t, path)
	defer func() { _ = db.Close() }()
	ctx := context.Background()

	if v, err := db.AppliedVersion(ctx); err != nil || v != 6 {
		t.Fatalf("pre-upgrade AppliedVersion = %d (err %v), want 6", v, err)
	}
	applyUptoVersion(t, db, 7)
	if v, err := db.AppliedVersion(ctx); err != nil || v != 7 {
		t.Fatalf("AppliedVersion = %d (err %v), want 7", v, err)
	}

	sources := map[string]string{}
	rows, err := db.SQL().QueryContext(ctx, `SELECT id, source FROM proxies`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, source string
		if err := rows.Scan(&id, &source); err != nil {
			t.Fatal(err)
		}
		sources[id] = source
	}
	_ = rows.Close()
	want := map[string]string{"p1": "subscription", "p2": "custom", "p3": "file"}
	for id, w := range want {
		if sources[id] != w {
			t.Errorf("proxy %s source = %q, want %q", id, sources[id], w)
		}
	}

	// Vault references, config and subscription links untouched.
	var ref, cfg, sub, nodeName string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT secret_ref, config_id, subscription_id, source_node_name FROM proxies WHERE id = 'p1'`,
	).Scan(&ref, &cfg, &sub, &nodeName); err != nil {
		t.Fatal(err)
	}
	if ref != "ref-p1" || cfg != "c1" || sub != "s1" || nodeName != "订阅节点" {
		t.Errorf("p1 = (%q, %q, %q, %q), want (ref-p1, c1, s1, 订阅节点)", ref, cfg, sub, nodeName)
	}
	// The instance binding to the renamed manual node survived.
	var bound *string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT proxy_id FROM browser_instances WHERE id = 'i1'`).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound == nil || *bound != "p2" {
		t.Errorf("instance proxy_id = %v, want p2", bound)
	}
	// Group members survived with order, kinds and targets.
	type member struct {
		kind, proxy, target, name string
	}
	got := map[string][]member{}
	mrows, err := db.SQL().QueryContext(ctx,
		`SELECT group_id, member_kind, COALESCE(proxy_id, ''), COALESCE(target_group_id, ''), member_name
		 FROM mihomo_proxy_group_members ORDER BY group_id, sort_order`)
	if err != nil {
		t.Fatal(err)
	}
	for mrows.Next() {
		var g string
		var m member
		if err := mrows.Scan(&g, &m.kind, &m.proxy, &m.target, &m.name); err != nil {
			t.Fatal(err)
		}
		got[g] = append(got[g], m)
	}
	_ = mrows.Close()
	if len(got["g1"]) != 2 || got["g1"][0] != (member{"proxy", "p1", "", "订阅节点"}) ||
		got["g1"][1] != (member{"builtin", "", "", "DIRECT"}) {
		t.Errorf("g1 members = %+v", got["g1"])
	}
	if len(got["g2"]) != 2 || got["g2"][0] != (member{"proxy", "p3", "", "文件节点"}) ||
		got["g2"][1] != (member{"group", "", "g1", "选择"}) {
		t.Errorf("g2 members = %+v", got["g2"])
	}
	// Foreign keys and integrity hold after the rebuild.
	var fk string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_key_check`).Scan(&fk); err != nil && fk != "" {
		t.Fatalf("foreign_key_check: %v %q", err, fk)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q (err %v)", integrity, err)
	}
	// The new CHECK rejects the retired vocabulary and the old
	// partial unique dedup index still applies.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, source) VALUES ('bad', '坏节点', 'socks5', 'manual')`); err == nil {
		t.Error("insert with retired source 'manual' succeeded")
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, source, dedup_key) VALUES ('dup', '重复', 'socks5', 'custom', 'dk-p1')`); err == nil {
		t.Error("duplicate dedup_key insert succeeded after rebuild")
	}
}

// TestFreshDatabaseReachesV7: a brand-new database migrates
// 0 -> 7 and new manual creations default to 'custom'.
func TestFreshDatabaseReachesV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	applyUptoVersion(t, db, 7)
	if v, err := db.AppliedVersion(ctx); err != nil || v != 7 {
		t.Fatalf("AppliedVersion = %d (err %v), want 7", v, err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol) VALUES ('n1', '新节点', 'socks5')`); err != nil {
		t.Fatalf("insert default-source proxy: %v", err)
	}
	var source string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT source FROM proxies WHERE id = 'n1'`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "custom" {
		t.Fatalf("default source = %q, want custom", source)
	}
}
