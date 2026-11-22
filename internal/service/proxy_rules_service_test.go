package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Tests for the editable rules and settings (migration 0017):
// policies, custom rules and settings all flow through the
// proposed-state -> regenerate -> persist discipline, so with
// the core stopped the observable artifact is the generated
// configuration file plus the store rows.

func readConfigFile(t *testing.T, f *proxyFixture) string {
	t.Helper()
	data, err := os.ReadFile(f.svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// addFileSourceOK adds a local-file source through the picker
// seam and returns its view.
func addFileSourceOK(t *testing.T, f *proxyFixture, name, content string) SourceView {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.SourceFilePicker = func() (string, error) { return path, nil }
	view, err := f.svc.AddFileSource("op", name)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestRulePolicyFlow(t *testing.T) {
	f := testProxyFixture(t)
	ctx := context.Background()
	inst := f.makeInstance(t, "实例一")
	src := addFileSourceOK(t, f, "源一", "proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: 18101}\n")
	if _, err := f.svc.SetBinding("op", inst.ID, src.ID, "NodeA"); err != nil {
		t.Fatal(err)
	}
	// Defaults: all fourteen views, lan direct, ad reject.
	views, err := f.svc.GetRulePolicies()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 14 {
		t.Fatalf("policies = %d, want 14", len(views))
	}
	// Disable the ad set and remap the LAN set to reject.
	if err := f.svc.SetRulePolicy("op", "禁止广告", "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetRulePolicy("op", "局域网络", "reject"); err != nil {
		t.Fatal(err)
	}
	cfg := readConfigFile(t, f)
	if strings.Contains(cfg, "禁止广告") {
		t.Fatalf("disabled ruleset must vanish from the config:\n%s", cfg)
	}
	if !strings.Contains(cfg, "RULE-SET,局域网络,REJECT") {
		t.Fatalf("remapped policy missing:\n%s", cfg)
	}
	stored, err := f.store.ListRulePolicies(ctx)
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored overrides = %v (err %v), want 2", stored, err)
	}
	// Restore defaults brings the ad set back.
	if err := f.svc.ResetRulePolicies("op"); err != nil {
		t.Fatal(err)
	}
	cfg = readConfigFile(t, f)
	if !strings.Contains(cfg, "RULE-SET,禁止广告,REJECT") || !strings.Contains(cfg, "RULE-SET,局域网络,DIRECT") {
		t.Fatalf("defaults not restored:\n%s", cfg)
	}
	if err := f.svc.SetRulePolicy("op", "不存在的集", "proxy"); err == nil {
		t.Fatal("unknown ruleset must be rejected")
	}
	if err := f.svc.SetRulePolicy("op", "局域网络", "bogus"); err == nil {
		t.Fatal("invalid policy must be rejected")
	}
}

func TestCustomRuleFlow(t *testing.T) {
	f := testProxyFixture(t)
	ctx := context.Background()
	inst := f.makeInstance(t, "实例一")
	src := addFileSourceOK(t, f, "源一", "proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: 18101}\n")
	if _, err := f.svc.SetBinding("op", inst.ID, src.ID, "NodeA"); err != nil {
		t.Fatal(err)
	}
	first, err := f.svc.AddCustomRule("op", "DOMAIN-SUFFIX", "example.com", "reject")
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.AddCustomRule("op", "IP-CIDR", "10.0.0.0/8", "proxy")
	if err != nil {
		t.Fatal(err)
	}
	cfg := readConfigFile(t, f)
	iFirst := strings.Index(cfg, "DOMAIN-SUFFIX,example.com,REJECT")
	iSecond := strings.Index(cfg, "IP-CIDR,10.0.0.0/8,inst-")
	iBuiltin := strings.Index(cfg, "RULE-SET,规则修正,DIRECT")
	if iFirst < 0 || iSecond < 0 || !(iFirst < iSecond && iSecond < iBuiltin) {
		t.Fatalf("custom rules must lead the sub-rules in order:\n%s", cfg)
	}
	if !strings.Contains(cfg, ",no-resolve") {
		t.Fatalf("IP-CIDR custom rule must gain no-resolve:\n%s", cfg)
	}
	// Reorder swaps them in the generated configuration too.
	if err := f.svc.ReorderCustomRules("op", []string{second.ID, first.ID}); err != nil {
		t.Fatal(err)
	}
	cfg = readConfigFile(t, f)
	if strings.Index(cfg, "IP-CIDR,10.0.0.0/8") > strings.Index(cfg, "DOMAIN-SUFFIX,example.com") {
		t.Fatalf("reorder not reflected:\n%s", cfg)
	}
	// Disabling via update removes the line but keeps the row.
	if err := f.svc.UpdateCustomRule("op", first.ID, "DOMAIN-SUFFIX", "example.com", "reject", false); err != nil {
		t.Fatal(err)
	}
	cfg = readConfigFile(t, f)
	if strings.Contains(cfg, "DOMAIN-SUFFIX,example.com") {
		t.Fatalf("disabled custom rule must not be generated:\n%s", cfg)
	}
	rules, err := f.svc.ListCustomRules()
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules = %v (err %v), want 2 rows kept", rules, err)
	}
	// Delete removes it everywhere.
	if err := f.svc.DeleteCustomRule("op", first.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.ListCustomRules(ctx)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored rules = %v (err %v), want 1", stored, err)
	}
	// Validation.
	for _, bad := range [][3]string{
		{"DOMAIN", "example.com", "proxy"},
		{"DOMAIN-SUFFIX", "https://example.com", "proxy"},
		{"DOMAIN-SUFFIX", ".example.com", "proxy"},
		{"IP-CIDR", "10.0.0.1", "proxy"},
		{"DOMAIN-KEYWORD", "a b", "proxy"},
		{"DOMAIN-SUFFIX", "example.com", "disabled"},
	} {
		if _, err := f.svc.AddCustomRule("op", bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("AddCustomRule(%v) must fail", bad)
		}
	}
}

func TestProxySettingsFlow(t *testing.T) {
	f := testProxyFixture(t)
	ctx := context.Background()
	// Defaults first.
	view, err := f.svc.GetProxySettings()
	if err != nil {
		t.Fatal(err)
	}
	if view.TestTimeoutMs != 5000 || view.LogLevel != "warning" || view.ListenerPortStart != 21001 {
		t.Fatalf("defaults = %+v", view)
	}
	next := ProxySettingsView{
		TestURL: "https://example.com/204", TestTimeoutMs: 3000,
		ProviderIntervalSec: 3600, DoHServers: []string{"https://dns.example/dns-query"},
		LogLevel: "info", ListenerPortStart: 22001,
	}
	if err := f.svc.UpdateProxySettings("op", next); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.GetProxySettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.TestURL != next.TestURL || got.LogLevel != "info" || got.ListenerPortStart != 22001 || len(got.DoHServers) != 1 {
		t.Fatalf("saved settings = %+v, want %+v", got, next)
	}
	// The generated configuration carries the embedded settings
	// (a bound rule-mode instance makes providers exist at all).
	inst := f.makeInstance(t, "实例一")
	src := addFileSourceOK(t, f, "源一", "proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: 18101}\n")
	if _, err := f.svc.SetBinding("op", inst.ID, src.ID, "NodeA"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.regenerate(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := readConfigFile(t, f)
	for _, want := range []string{"log-level: info", "https://dns.example/dns-query", "interval: 3600"} {
		if !strings.Contains(cfg, want) {
			t.Fatalf("config missing %q:\n%s", want, cfg)
		}
	}
	// Invalid updates are rejected and change nothing.
	bad := next
	bad.TestTimeoutMs = 10
	if err := f.svc.UpdateProxySettings("op", bad); err == nil {
		t.Fatal("out-of-range timeout must be rejected")
	}
	bad = next
	bad.DoHServers = []string{"http://insecure.example/dns-query"}
	if err := f.svc.UpdateProxySettings("op", bad); err == nil {
		t.Fatal("non-https DoH must be rejected")
	}
	after, err := f.svc.GetProxySettings()
	if err != nil || after.TestTimeoutMs != 3000 {
		t.Fatalf("settings changed by a rejected update: %+v (err %v)", after, err)
	}
	_ = model.DefaultProxySettings
}

// TestReorderCustomRulesRejectsBadSets covers the service-side
// set validation: duplicate, missing and unknown ids are all
// refused and the stored order survives every refusal.
func TestReorderCustomRulesRejectsBadSets(t *testing.T) {
	f := testProxyFixture(t)
	ctx := context.Background()
	r1, err := f.svc.AddCustomRule("op", "DOMAIN-SUFFIX", "a.com", "proxy")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f.svc.AddCustomRule("op", "DOMAIN-SUFFIX", "b.com", "direct")
	if err != nil {
		t.Fatal(err)
	}
	r3, err := f.svc.AddCustomRule("op", "DOMAIN-SUFFIX", "c.com", "reject")
	if err != nil {
		t.Fatal(err)
	}
	_ = ctx

	bad := map[string][]string{
		"duplicate": {r1.ID, r1.ID, r2.ID},
		"missing":   {r1.ID, r2.ID},
		"unknown":   {r1.ID, r2.ID, "no-such-rule"},
		"extra":     {r1.ID, r2.ID, r3.ID, "no-such-rule"},
	}
	for name, ids := range bad {
		if err := f.svc.ReorderCustomRules("op", ids); err == nil {
			t.Fatalf("%s: want an error", name)
		}
		rules, err := f.svc.ListCustomRules()
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != 3 || rules[0].ID != r1.ID || rules[1].ID != r2.ID || rules[2].ID != r3.ID {
			t.Fatalf("%s: order changed after a rejected reorder: %+v", name, rules)
		}
	}

	// A valid full reorder applies.
	if err := f.svc.ReorderCustomRules("op", []string{r3.ID, r1.ID, r2.ID}); err != nil {
		t.Fatal(err)
	}
	rules, err := f.svc.ListCustomRules()
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].ID != r3.ID || rules[1].ID != r1.ID || rules[2].ID != r2.ID {
		t.Fatalf("order after valid reorder = %+v", rules)
	}
}
