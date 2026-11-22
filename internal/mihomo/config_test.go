package mihomo

import (
	"strings"
	"testing"
)

func TestGenerateShape(t *testing.T) {
	out := Generate(
		[]NodeDef{
			{CoreName: "n-bbb", Config: map[string]any{"name": "节点B", "type": "socks5", "server": "127.0.0.1", "port": float64(18102)}},
			{CoreName: "n-aaa", Config: map[string]any{"name": "节点A", "type": "socks5", "server": "127.0.0.1", "port": float64(18101)}},
		},
		[]BindingDef{
			{Port: 21002, NodeCoreName: "n-bbb"},
			{Port: 21001, NodeCoreName: "n-aaa"},
		},
		19097, "s3cret",
	)
	for _, want := range []string{
		"external-controller: 127.0.0.1:19097",
		`secret: "s3cret"`,
		`"name": "n-aaa"`, // core-side names replace display names
		`"name": "n-bbb"`,
		"port: 21001",
		"port: 21002",
		`proxy: "n-aaa"`,
		`proxy: "n-bbb"`,
		"listen: 127.0.0.1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %q:\n%s", want, out)
		}
	}
	// Deterministic: nodes sorted by core name, listeners by port.
	if strings.Index(out, "n-aaa") > strings.Index(out, "n-bbb") {
		t.Errorf("nodes not sorted:\n%s", out)
	}
	if strings.Index(out, "port: 21001") > strings.Index(out, "port: 21002") {
		t.Errorf("listeners not sorted:\n%s", out)
	}
	// The forbidden machinery of the old design stays out.
	for _, banned := range []string{"rules:", "rule-providers", "proxy-groups", "sub-rules", "dns:"} {
		if strings.Contains(out, banned) {
			t.Errorf("generated config must not contain %q:\n%s", banned, out)
		}
	}
}

func TestGenerateEmpty(t *testing.T) {
	out := Generate(nil, nil, 19097, "x")
	if !strings.Contains(out, "proxies: []") || !strings.Contains(out, "listeners: []") {
		t.Fatalf("empty state must still render a valid config:\n%s", out)
	}
}
