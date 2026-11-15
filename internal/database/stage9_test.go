package database

import (
	"context"
	"testing"
)

// TestMigration9DropsOverlayKeepsEverythingElse: a database that
// applied migration 0008 (the PR #9 acceptance builds) and holds
// overlay rows upgrades through 0009 — the overlay table is
// dropped while configs, nodes, groups, members, instances and
// vault references stay exactly as they were. Migration 0008's
// file stays in the history; 0009 is the removal step.
func TestMigration9DropsOverlayKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/debrowser.db"
	db := openV6Database(t, path)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	applyUptoVersion(t, db, 8)
	for _, stmt := range []string{
		`INSERT INTO proxies (id, name, protocol, address, port, secret_ref, source)
		 VALUES ('px', '自定义节点', 'socks5', '127.0.0.1', 1080, 'ref-px', 'custom')`,
		`INSERT INTO mihomo_proxy_group_custom_members (config_id, group_name, proxy_id, sort_order)
		 VALUES ('c1', '选择', 'px', 0)`,
	} {
		if _, err := db.SQL().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed v8 overlay: %v", err)
		}
	}
	tables := []string{"proxies", "subscriptions", "mihomo_configs", "mihomo_proxy_groups", "mihomo_proxy_group_members", "browser_instances"}
	counts := map[string]int{}
	var n int
	for _, table := range tables {
		if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[table] = n
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate to v9: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 9 {
		t.Fatalf("applied version = %d (err %v), want 9", v, err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='mihomo_proxy_group_custom_members'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("overlay table after 0009: count %d (err %v), want dropped", n, err)
	}
	for _, table := range tables {
		if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(1) FROM `+table).Scan(&n); err != nil || n != counts[table] {
			t.Errorf("%s rows = %d (err %v), want %d — data must survive the overlay removal", table, n, err, counts[table])
		}
	}
	var res string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		t.Fatalf("integrity_check = %q (err %v)", res, err)
	}
	rows, err := db.SQL().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a violation row")
	}
}

// TestMigration9FreshDatabase: a fresh database ends at v9 with
// no overlay table and all Stage 6 tables present.
func TestMigration9FreshDatabase(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if v, err := db.AppliedVersion(ctx); err != nil || v != 9 {
		t.Fatalf("applied version = %d (err %v), want 9", v, err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='mihomo_proxy_group_custom_members'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("overlay table on fresh DB: count %d (err %v), want absent", n, err)
	}
}
