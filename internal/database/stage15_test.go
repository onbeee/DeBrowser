package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 6C: migration 0015 adds proxy_sources and
// instance_proxy_bindings. Historical tables stay untouched.

func TestMigration15ProxyTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v, err := db.AppliedVersion(ctx); err != nil || v != 16 {
		t.Fatalf("AppliedVersion = %d (err %v), want 16", v, err)
	}
	for _, table := range []string{"proxy_sources", "instance_proxy_bindings"} {
		var name string
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}

	// One instance, one source, one binding.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO browser_instances (id, name, user_data_dir) VALUES ('i1', '实例一', 'profiles/i1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_sources (id, name, kind, provider_name) VALUES ('s1', '订阅一', 'url', 'src-00000001')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO instance_proxy_bindings (instance_id, source_id, node_name, port)
		 VALUES ('i1', 's1', '香港 01', 21001)`); err != nil {
		t.Fatal(err)
	}
	// A source with bindings cannot be deleted (FK without cascade).
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM proxy_sources WHERE id = 's1'`); err == nil {
		t.Fatal("deleting a source with bindings must fail at the FK level")
	}
	// Deleting the instance cascades its binding away.
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM browser_instances WHERE id = 'i1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(1) FROM instance_proxy_bindings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bindings after instance delete = %d (err %v), want 0", n, err)
	}
	// Now the source can go.
	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM proxy_sources WHERE id = 's1'`); err != nil {
		t.Fatalf("delete source after cascade: %v", err)
	}
	// The kind CHECK constraint holds.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_sources (id, name, kind, provider_name) VALUES ('s2', 'x', 'bogus', 'src-2')`); err == nil {
		t.Fatal("invalid source kind must fail the CHECK constraint")
	}
}
