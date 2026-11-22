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
		// Listeners hand their traffic straight to the instance's
		// own group.
		"type: mixed",
		"port: 21001",
		"port: 21002",
		"proxy: \"inst-111111112222\"",
		"proxy: \"inst-666666667777\"",
		// Top-level fallback is DIRECT, never REJECT.
		"rules:\n  - \"MATCH,DIRECT\"",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, out)
		}
	}
	// DeBrowser keeps no rule set of its own: no sub-rules, no
	// built-in direct/bypass rule lines anywhere. The system DNS
	// resolver must not reappear as an upstream either.
	for _, banned := range []string{"sub-rules:", "IP-CIDR", "DOMAIN-SUFFIX", "\n    rule: ", "- system"} {
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
	if !strings.Contains(out, "\"MATCH,DIRECT\"") {
		t.Errorf("empty config still needs the top-level fallback\n%s", out)
	}
}
