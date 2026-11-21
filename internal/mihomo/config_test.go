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
			{InstanceID: "11111111-2222-3333-4444-555555555555", ProviderName: "src-aaaa1111", NodeName: "香港 01", Mode: "rule", Port: 21002},
			{InstanceID: "66666666-7777-8888-9999-000000000000", ProviderName: "src-bbbb2222", NodeName: "NodeA", Mode: "global", Port: 21001},
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
		"external-controller: \"127.0.0.1:19097\"",
		"secret: \"test-secret\"",
		"store-selected: false",
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
		// Per-instance groups, each fed by its own provider only.
		"\"inst-111111112222\"",
		"\"inst-666666667777\"",
		// Listeners bind loopback and reference sub-rules, never proxy:.
		"type: mixed",
		"port: 21001",
		"port: 21002",
		// Rule-mode instance: private ranges direct, MATCH to own group.
		"\"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve\"",
		"\"MATCH,inst-111111112222\"",
		// Global-mode instance: single MATCH line to its own group.
		"\"MATCH,inst-666666667777\"",
		// Top-level fallback is DIRECT, never REJECT.
		"rules:\n  - \"MATCH,DIRECT\"",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "proxy: \"") || strings.Contains(out, "\n    proxy: NodeA") {
		t.Errorf("listener must not use proxy: binding\n%s", out)
	}
	// The global-mode instance must not inherit the default rule lines.
	if strings.Count(out, "IP-CIDR,192.168.0.0/16,DIRECT,no-resolve") != 1 {
		t.Errorf("default rule lines should appear exactly once (rule-mode instance only)\n%s", out)
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
	bad.Bindings = []BindingInput{{InstanceID: "x", ProviderName: "src-nope", NodeName: "N", Mode: "rule", Port: 21001}}
	if _, err := Generate(bad); err == nil {
		t.Error("binding to unknown provider must fail")
	}
	bad = base
	bad.Bindings = []BindingInput{{InstanceID: "x", ProviderName: "src-aaaa1111", NodeName: "N", Mode: "weird", Port: 21001}}
	if _, err := Generate(bad); err == nil {
		t.Error("invalid mode must fail")
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
