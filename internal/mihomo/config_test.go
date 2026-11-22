package mihomo

import (
	"strings"
	"testing"
)

func sampleInput() ConfigInput {
	return ConfigInput{
		ControllerPort: 19097,
		Secret:         "test-secret",
		Sources: []SourceInput{
			{ProviderName: "src-bbbb2222", Kind: "file", FileName: "f2.yaml"},
			{ProviderName: "src-aaaa1111", Kind: "url", URL: "https://sub.example.com/feed?token=a&b=\"quoted\""},
		},
		Bindings: []BindingInput{
			{InstanceID: "11111111-2222-3333-4444-555555555555", ProviderName: "src-aaaa1111", NodeName: "香港 01", Port: 21002},
			{InstanceID: "66666666-7777-8888-9999-000000000000", ProviderName: "src-bbbb2222", NodeName: "NodeA", Port: 21001},
		},
	}
}

func TestGenerateShape(t *testing.T) {
	out, err := Generate(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	mustContain := []string{
		"mode: rule\n",
		"ipv6: false\n",
		"tcp-concurrent: true\n",
		"unified-delay: true\n",
		"external-controller: \"127.0.0.1:19097\"",
		"secret: \"test-secret\"",
		"store-selected: false",
		// DNS: redir-host with DoH upstreams; the system resolver
		// must not come back (it stalls first queries under
		// redir-host and fails concurrent delay tests).
		"enhanced-mode: redir-host",
		"default-nameserver:\n    - \"223.5.5.5\"\n    - \"223.6.6.6\"",
		"proxy-server-nameserver:\n    - \"https://223.5.5.5/dns-query\"\n    - \"https://223.6.6.6/dns-query\"",
		"nameserver:\n    - \"https://223.5.5.5/dns-query\"\n    - \"https://223.6.6.6/dns-query\"",
		// URL source: http provider, fetch pinned to DIRECT.
		"src-aaaa1111:\n    type: http",
		"proxy: DIRECT",
		"./providers/src-aaaa1111.yaml",
		// File source.
		"src-bbbb2222:\n    type: file",
		"./sources/f2.yaml",
		// Hidden per-source delay groups.
		"\"delay-src-aaaa1111\"",
		"\"delay-src-bbbb2222\"",
		// Per-instance groups, each fed by its own provider only,
		// with REJECT as the first (default) member so a group
		// whose selected node disappears refuses traffic instead
		// of falling back to another node.
		"\"inst-111111112222\"",
		"\"inst-666666667777\"",
		"proxies:\n      - REJECT",
		// Listeners hand their traffic to the instance's own
		// sub-rules, never straight to a group.
		"type: mixed",
		"port: 21001",
		"port: 21002",
		"rule: \"sub-inst-111111112222\"",
		"rule: \"sub-inst-666666667777\"",
		// Both sample bindings are route-mode rule: the shared
		// rule-providers and per-instance sub-rules are emitted.
		"rule-providers:",
		"\"规则修正\":\n    type: http\n    behavior: classical\n    format: yaml",
		"url: \"https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Direct/Direct.yaml\"",
		"path: \"./rules/direct.yaml\"",
		"\"局域网络\":",
		"path: \"./rules/lan.yaml\"",
		"interval: 86400",
		"sub-rules:",
		"\"sub-inst-111111112222\":",
		"\"RULE-SET,规则修正,DIRECT\"",
		"\"RULE-SET,禁止广告,REJECT\"",
		"\"RULE-SET,智能服务,inst-111111112222\"",
		"\"RULE-SET,国际服务,inst-111111112222\"",
		"\"RULE-SET,国内服务,DIRECT\"",
		"\"RULE-SET,局域网络,DIRECT\"",
		"\"GEOIP,CN,DIRECT,no-resolve\"",
		// Top-level fallback is DIRECT, never REJECT.
		"rules:\n  - \"MATCH,DIRECT\"",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, out)
		}
	}
	// All fourteen shared rule-providers, by name.
	for _, name := range []string{"规则修正", "屏蔽服务", "禁止广告", "反劫服务", "隐私保护", "智能服务", "谷歌服务", "推特服务", "飞机服务", "加密服务", "微软服务", "国际服务", "国内服务", "局域网络"} {
		if !strings.Contains(out, "\""+name+"\":") {
			t.Errorf("generated config missing rule-provider %q\n---\n%s", name, out)
		}
	}
	// The rule-mode sub-rules end on the instance's own group.
	idx := strings.Index(out, "\"sub-inst-666666667777\":")
	if idx < 0 {
		t.Fatalf("sub-rules for the second instance missing\n---\n%s", out)
	}
	block := out[idx:]
	if !strings.HasPrefix(block, "\"sub-inst-666666667777\":\n    - \"RULE-SET,规则修正,DIRECT\"") {
		t.Errorf("rule-mode sub-rules must start with the direct-fix set\n---\n%s", block)
	}
	if !strings.Contains(block, "- \"MATCH,inst-666666667777\"") {
		t.Errorf("rule-mode sub-rules must end on the instance group\n---\n%s", block)
	}
	// Listeners must not bypass the sub-rules via proxy:, and the
	// system DNS resolver must not reappear as an upstream.
	for _, banned := range []string{"proxy: \"inst-", "- system"} {
		if strings.Contains(out, banned) {
			t.Errorf("generated config must not contain %q\n---\n%s", banned, out)
		}
	}
	// REJECT appears exactly once per instance group (2 groups).
	if n := strings.Count(out, "- REJECT"); n != 2 {
		t.Errorf("REJECT members = %d, want 2 (one per instance group)\n%s", n, out)
	}
}

func TestGenerateEscapesURL(t *testing.T) {
	out, err := Generate(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	// The token contains & and a double quote; the emitted scalar
	// must be a JSON-quoted string so the value cannot break out.
	if !strings.Contains(out, `url: "https://sub.example.com/feed?token=a&b=\"quoted\""`) {
		t.Errorf("subscription URL not safely quoted\n%s", out)
	}
}

func TestGenerateErrors(t *testing.T) {
	base := sampleInput()
	bad := base
	bad.Secret = ""
	if _, err := Generate(bad); err == nil {
		t.Error("empty secret must fail")
	}
	bad = base
	bad.Bindings = []BindingInput{{InstanceID: "x", ProviderName: "src-nope", NodeName: "N", Port: 21001}}
	if _, err := Generate(bad); err == nil {
		t.Error("binding to unknown provider must fail")
	}
	bad = base
	bad.Sources = []SourceInput{{ProviderName: "src-x", Kind: "carrier-pigeon"}}
	if _, err := Generate(bad); err == nil {
		t.Error("invalid source kind must fail")
	}
}

func TestGenerateEmpty(t *testing.T) {
	out, err := Generate(ConfigInput{ControllerPort: 19097, Secret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "proxy-providers:") || strings.Contains(out, "listeners:") {
		t.Errorf("empty input must not emit providers or listeners\n%s", out)
	}
	if strings.Contains(out, "rule-providers:") || strings.Contains(out, "sub-rules:") {
		t.Errorf("empty input must not emit rule-providers or sub-rules\n%s", out)
	}
	if !strings.Contains(out, "\"MATCH,DIRECT\"") {
		t.Errorf("empty config still needs the top-level fallback\n%s", out)
	}
}

func TestGenerateEditableRules(t *testing.T) {
	in := sampleInput()                 // binding 0 (instance 1111…) is rule mode; its group is inst-111111112222
	in.Bindings[1].RouteMode = "global" // the other instance: customs must not leak in
	in.CustomRules = []CustomRuleInput{
		{RuleType: "DOMAIN-SUFFIX", Value: "example.com", Policy: "reject", Enabled: true},
		{RuleType: "IP-CIDR", Value: "10.0.0.0/8", Policy: "proxy", Enabled: true},
		{RuleType: "DOMAIN-KEYWORD", Value: "tracker", Policy: "direct", Enabled: false},
	}
	in.RulePolicies = map[string]string{
		"局域网络": "reject",   // default direct -> reject
		"禁止广告": "disabled", // default reject -> not generated at all
		"智能服务": "direct",   // default proxy -> direct
	}
	out, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	sub := subRulesBlock(t, out, SubRuleName("11111111-2222-3333-4444-555555555555"))
	// Custom rules come first, in order, enabled only; IP-CIDR
	// gains ,no-resolve; proxy maps to the instance's own group.
	wantHead := []string{
		"DOMAIN-SUFFIX,example.com,REJECT",
		"IP-CIDR,10.0.0.0/8,inst-111111112222,no-resolve",
	}
	for i, want := range wantHead {
		if sub[i] != want {
			t.Fatalf("sub-rule %d = %q, want %q\nall: %v", i, sub[i], want, sub)
		}
	}
	if strings.Contains(out, "tracker") {
		t.Errorf("a disabled custom rule must not be generated\n%s", out)
	}
	// The disabled ruleset is skipped in BOTH the providers and
	// the sub-rules (a dangling reference would break the core).
	if strings.Contains(out, "禁止广告") {
		t.Errorf("a disabled ruleset must not appear anywhere\n%s", out)
	}
	// Remapped policies land in the sub-rules.
	for _, want := range []string{"RULE-SET,局域网络,REJECT", "RULE-SET,智能服务,DIRECT"} {
		found := false
		for _, line := range sub {
			if line == want {
				found = true
			}
		}
		if !found {
			t.Errorf("sub-rules missing %q\nall: %v", want, sub)
		}
	}
	// The fixed tail stays: GEOIP then MATCH onto the group.
	if sub[len(sub)-2] != "GEOIP,CN,DIRECT,no-resolve" || sub[len(sub)-1] != "MATCH,inst-111111112222" {
		t.Errorf("sub-rule tail = %v", sub[len(sub)-2:])
	}
	// Custom rules never leak into a global-mode instance's
	// sub-rules: it keeps exactly its single MATCH line.
	gb := subRulesBlock(t, out, SubRuleName("66666666-7777-8888-9999-000000000000"))
	if len(gb) != 1 || gb[0] != "MATCH,inst-666666667777" {
		t.Errorf("global sub-rules = %v, want the single MATCH line", gb)
	}
}

func TestGenerateEditableSettings(t *testing.T) {
	in := sampleInput()
	in.LogLevel = "info"
	in.DoHServers = []string{"https://dns.example/dns-query"}
	in.ProviderIntervalSec = 3600
	out, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"log-level: info\n",
		"    - \"https://dns.example/dns-query\"\n",
		"    interval: 3600\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	if strings.Count(out, "https://dns.example/dns-query") != 2 {
		t.Errorf("the DoH list must feed both nameserver lists\n%s", out)
	}
	if strings.Count(out, "interval: 3600") < 2 || strings.Contains(out, "interval: 86400") {
		t.Errorf("the interval must feed proxy-providers and rule-providers alike\n%s", out)
	}
	for _, bad := range []ConfigInput{
		{ControllerPort: 19097, Secret: "s", LogLevel: "trace"},
		{ControllerPort: 19097, Secret: "s", CustomRules: []CustomRuleInput{{RuleType: "DOMAIN", Value: "x", Policy: "proxy", Enabled: true}}},
		{ControllerPort: 19097, Secret: "s", CustomRules: []CustomRuleInput{{RuleType: "DOMAIN-SUFFIX", Value: "a,b", Policy: "proxy", Enabled: true}}},
		{ControllerPort: 19097, Secret: "s", RulePolicies: map[string]string{"局域网络": "bogus"}},
	} {
		if _, err := Generate(bad); err == nil {
			t.Errorf("Generate(%+v) must fail", bad)
		}
	}
}

// subRulesBlock extracts one instance's sub-rule lines from the
// generated configuration.
func subRulesBlock(t *testing.T, out, name string) []string {
	t.Helper()
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if l == "  "+yq(name)+":" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("sub-rules for %s not found\n%s", name, out)
	}
	var sub []string
	for _, l := range lines[start:] {
		if !strings.HasPrefix(l, "    - ") {
			break
		}
		sub = append(sub, strings.Trim(l, "    - \""))
	}
	return sub
}

func TestGenerateRouteModes(t *testing.T) {
	in := sampleInput()
	in.Bindings[0].RouteMode = "global"
	in.Bindings[1].RouteMode = "direct"
	out, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	// No rule-mode instance: the shared rule-providers are not
	// emitted at all.
	if strings.Contains(out, "rule-providers:") {
		t.Errorf("no rule-mode binding must not emit rule-providers\n%s", out)
	}
	// Global: the sub-rules are exactly the single MATCH to the
	// instance's own group.
	if !strings.Contains(out, "\"sub-inst-111111112222\":\n    - \"MATCH,inst-111111112222\"\n") {
		t.Errorf("global-mode sub-rules wrong\n%s", out)
	}
	// Direct: exactly MATCH,DIRECT.
	if !strings.Contains(out, "\"sub-inst-666666667777\":\n    - \"MATCH,DIRECT\"\n") {
		t.Errorf("direct-mode sub-rules wrong\n%s", out)
	}
	if strings.Contains(out, "RULE-SET") {
		t.Errorf("global/direct sub-rules must not reference rule sets\n%s", out)
	}

	// One rule-mode instance among others brings the shared
	// providers back.
	in.Bindings[1].RouteMode = "rule"
	out, err = Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "rule-providers:") || !strings.Contains(out, "\"RULE-SET,局域网络,DIRECT\"") {
		t.Errorf("one rule-mode binding must emit the shared rule-providers\n%s", out)
	}

	// An invalid mode is rejected, never silently mapped.
	in.Bindings[1].RouteMode = "bogus"
	if _, err := Generate(in); err == nil {
		t.Error("invalid route mode must fail")
	}
}
