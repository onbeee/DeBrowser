package database

import (
	"context"
	"path/filepath"
	"testing"
)

// Stage 6C: migration 0017 adds the editable-rules and settings
// tables (proxy_rule_policies, proxy_custom_rules,
// proxy_settings) with their CHECK constraints.

func TestMigration17ProxyRulesSettings(t *testing.T) {
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
	if v, err := db.AppliedVersion(ctx); err != nil || v != 17 {
		t.Fatalf("AppliedVersion = %d (err %v), want 17", v, err)
	}
	// Policy overrides accept the four policies and reject others.
	for _, p := range []string{"proxy", "direct", "reject", "disabled"} {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO proxy_rule_policies (provider_name, policy) VALUES (?, ?)`, "集"+p, p); err != nil {
			t.Fatalf("policy %q: %v", p, err)
		}
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_rule_policies (provider_name, policy) VALUES ('x', 'bogus')`); err == nil {
		t.Fatal("an invalid ruleset policy must be rejected by the CHECK constraint")
	}
	// Custom rules accept the three types and three policies.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_custom_rules (id, rule_type, value, policy, sort_order)
		 VALUES ('r1', 'IP-CIDR', '10.0.0.0/8', 'reject', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_custom_rules (id, rule_type, value, policy, sort_order)
		 VALUES ('r2', 'DOMAIN', 'example.com', 'proxy', 1)`); err == nil {
		t.Fatal("an unsupported rule type must be rejected by the CHECK constraint")
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_custom_rules (id, rule_type, value, policy, sort_order)
		 VALUES ('r3', 'DOMAIN-SUFFIX', 'example.com', 'disabled', 1)`); err == nil {
		t.Fatal("custom rules must not accept the 'disabled' policy")
	}
	// Settings are plain key/value rows.
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO proxy_settings (key, value) VALUES ('log_level', 'info')`); err != nil {
		t.Fatal(err)
	}
}
