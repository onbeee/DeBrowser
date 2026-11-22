package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 6C: migration 0016 adds route_mode to
// instance_proxy_bindings (default 'rule', CHECK-constrained).

func TestMigration16BindingRouteMode(t *testing.T) {
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
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO browser_instances (id, name, user_data_dir) VALUES ('i1', '实例一', 'profiles/i1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_sources (id, name, kind, provider_name) VALUES ('s1', '订阅一', 'url', 'src-00000001')`); err != nil {
		t.Fatal(err)
	}
	// A binding inserted without route_mode takes the default.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO instance_proxy_bindings (instance_id, source_id, node_name, port)
		 VALUES ('i1', 's1', '香港 01', 21001)`); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT route_mode FROM instance_proxy_bindings WHERE instance_id = 'i1'`).Scan(&mode); err != nil || mode != "rule" {
		t.Fatalf("default route_mode = %q (err %v), want rule", mode, err)
	}
	// The other two modes are accepted.
	for _, m := range []string{"global", "direct"} {
		if _, err := db.SQL().ExecContext(ctx,
			`UPDATE instance_proxy_bindings SET route_mode = ? WHERE instance_id = 'i1'`, m); err != nil {
			t.Fatalf("route_mode %q must be accepted: %v", m, err)
		}
	}
	// Anything else fails the CHECK constraint.
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE instance_proxy_bindings SET route_mode = 'bogus' WHERE instance_id = 'i1'`); err == nil {
		t.Fatal("invalid route_mode must fail the CHECK constraint")
	}
}
