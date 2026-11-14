package database

import (
	"context"
	"path/filepath"
	"testing"
)

// TestUpgradeFromV7ToV8CustomMembers: migration 0008 only adds
// the custom group-member overlay table; every Stage 4A–6C row,
// relation and vault reference carries over untouched, and the
// overlay's foreign keys clean up after config/node deletion
// without ever cascading into other data.
func TestUpgradeFromV7ToV8CustomMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db := openV6Database(t, path)
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	applyUptoVersion(t, db, 7)
	if v, err := db.AppliedVersion(ctx); err != nil || v != 7 {
		t.Fatalf("pre-upgrade AppliedVersion = %d (err %v), want 7", v, err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (v7 -> v8): %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 8 {
		t.Fatalf("AppliedVersion = %d (err %v), want 8", v, err)
	}

	// The v7 state survived: vocabulary mapped, links intact.
	var source string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT source FROM proxies WHERE id = 'p2'`).Scan(&source); err != nil || source != "custom" {
		t.Fatalf("p2 source = %q (err %v), want custom", source, err)
	}
	var members int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM mihomo_proxy_group_members`).Scan(&members); err != nil || members != 4 {
		t.Fatalf("imported members = %d (err %v), want 4", members, err)
	}
	// Overlay table exists and starts empty.
	var overlay int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM mihomo_proxy_group_custom_members`).Scan(&overlay); err != nil || overlay != 0 {
		t.Fatalf("overlay rows = %d (err %v), want 0", overlay, err)
	}

	// Add a custom node and an overlay row at the database level.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, address, port, secret_ref, source)
		 VALUES ('px', '自定义节点', 'socks5', '127.0.0.1', 1080, 'ref-px', 'custom')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO mihomo_proxy_group_custom_members (config_id, group_name, proxy_id, sort_order)
		 VALUES ('c1', '选择', 'px', 0)`); err != nil {
		t.Fatal(err)
	}
	// The composite key rejects a duplicate.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO mihomo_proxy_group_custom_members (config_id, group_name, proxy_id, sort_order)
		 VALUES ('c1', '选择', 'px', 1)`); err == nil {
		t.Error("duplicate overlay row accepted")
	}
	// Deleting the node cleans only its overlay rows.
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM proxies WHERE id = 'px'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM mihomo_proxy_group_custom_members`).Scan(&overlay); err != nil || overlay != 0 {
		t.Fatalf("overlay rows after node delete = %d (err %v), want 0", overlay, err)
	}
	// Re-add, then deleting the config cleans them too — while the
	// config's imported groups are cascade-owned as before and no
	// other config's data is touched.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, source) VALUES ('py', '自定义二', 'socks5', 'custom')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO mihomo_proxy_group_custom_members (config_id, group_name, proxy_id) VALUES ('c2', '自动', 'py')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM mihomo_configs WHERE id = 'c2'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM mihomo_proxy_group_custom_members`).Scan(&overlay); err != nil || overlay != 0 {
		t.Fatalf("overlay rows after config delete = %d (err %v), want 0", overlay, err)
	}
	var pySource string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT source FROM proxies WHERE id = 'py'`).Scan(&pySource); err != nil || pySource != "custom" {
		t.Fatalf("custom node after config delete: source = %q (err %v)", pySource, err)
	}
	var fk string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_key_check`).Scan(&fk); err != nil && fk != "" {
		t.Fatalf("foreign_key_check: %v %q", err, fk)
	}
	var integrity string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q (err %v)", integrity, err)
	}
}

// TestFreshDatabaseReachesV8: a brand-new database migrates
// 0 -> 8 with the overlay table present.
func TestFreshDatabaseReachesV8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 8 {
		t.Fatalf("AppliedVersion = %d (err %v), want 8", v, err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name='mihomo_proxy_group_custom_members'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("overlay table exists = %d (err %v), want 1", n, err)
	}
}
