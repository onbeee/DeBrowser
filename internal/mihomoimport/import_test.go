package mihomoimport

import (
	"encoding/base64"
	"strings"
	"testing"
)

const fullConfig = `
port: 7890
allow-lan: true
external-controller: 0.0.0.0:9090
log-level: info
dns:
  enable: true
  listen: 0.0.0.0:53
  nameserver:
    - https://dns.example.com/dns-query
proxies:
  - name: "香港 SS"
    type: ss
    server: hk.example.com
    port: 8388
    cipher: aes-256-gcm
    password: "ss-secret-pw"
    udp: true
  - name: "日本 VMess"
    type: vmess
    server: jp.example.com
    port: 443
    uuid: "11111111-2222-3333-4444-555555555555"
    alterId: 0
    cipher: auto
    network: ws
    ws-opts:
      path: /ws
      headers:
        Host: cdn.example.com
  - name: "美国 Trojan"
    type: trojan
    server: us.example.com
    port: 443
    password: "trojan-secret"
    sni: us.example.com
proxy-groups:
  - name: "自动选择"
    type: url-test
    url: http://www.gstatic.com/generate_204
    interval: 300
    proxies:
      - "香港 SS"
      - "日本 VMess"
  - name: "总组"
    type: select
    proxies:
      - "自动选择"
      - "美国 Trojan"
      - DIRECT
rules:
  - MATCH,总组
`

func TestParseFullConfig(t *testing.T) {
	p, err := Parse([]byte(fullConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Format != FormatFullYAML {
		t.Fatalf("format = %v, want full", p.Format)
	}
	if len(p.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(p.Nodes))
	}
	ss := p.Nodes[0]
	if ss.Protocol != "shadowsocks" || ss.Server != "hk.example.com" || ss.Port != 8388 {
		t.Fatalf("ss node = %+v", ss)
	}
	if ss.Secrets["password"] != "ss-secret-pw" {
		t.Fatalf("ss secret not extracted: %v", ss.Secrets)
	}
	if _, leaked := ss.Config["password"]; leaked {
		t.Fatal("password leaked into node config")
	}
	if ss.Config["cipher"] != "aes-256-gcm" || ss.Config["udp"] != true {
		t.Fatalf("ss config = %v", ss.Config)
	}
	vm := p.Nodes[1]
	if vm.Secrets["uuid"] == "" {
		t.Fatal("vmess uuid not extracted")
	}
	if len(p.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(p.Groups))
	}
	if got := p.Groups[1].Members; len(got) != 3 || got[0] != "自动选择" || got[2] != "DIRECT" {
		t.Fatalf("group members = %v", got)
	}
	// Sanitized artifact: no plaintext secrets anywhere, structure
	// and unknown sections preserved.
	san := string(p.SanitizedYAML)
	for _, secret := range []string{"ss-secret-pw", "trojan-secret", "11111111-2222-3333-4444-555555555555"} {
		if strings.Contains(san, secret) {
			t.Fatalf("sanitized YAML leaks %q", secret)
		}
	}
	for _, keep := range []string{"MATCH", "dns", "香港 SS", Placeholder} {
		if !strings.Contains(san, keep) {
			t.Fatalf("sanitized YAML lost %q:\n%s", keep, san)
		}
	}
	// Security report: allow-lan, external controller, missing
	// controller secret, external DNS listen.
	codes := map[string]bool{}
	for _, issue := range p.Issues {
		codes[issue.Code] = true
	}
	for _, want := range []string{"allow_lan", "external_controller", "controller_no_secret", "dns_external_listen"} {
		if !codes[want] {
			t.Fatalf("missing security issue %q in %v", want, p.Issues)
		}
	}
}

func TestParseFullConfigTopLevelSecretToBundle(t *testing.T) {
	doc := "secret: ctrl-secret-value\n" +
		"external-controller: 127.0.0.1:9090\n" +
		"proxies:\n  - name: A\n    type: socks5\n    server: 10.0.0.1\n    port: 1080\n    username: bob\n    password: bob-pw\n"
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Bundle["secret"] != "ctrl-secret-value" {
		t.Fatalf("bundle = %v", p.Bundle)
	}
	if strings.Contains(string(p.SanitizedYAML), "ctrl-secret-value") {
		t.Fatal("sanitized YAML leaks the controller secret")
	}
	if strings.Contains(string(p.SanitizedYAML), "bob-pw") {
		t.Fatal("sanitized YAML leaks the node password")
	}
	node := p.Nodes[0]
	if node.Secrets["username"] != "bob" || node.Secrets["password"] != "bob-pw" {
		t.Fatalf("node secrets = %v", node.Secrets)
	}
	// Localhost controller WITH a secret: no findings.
	if len(p.Issues) != 0 {
		t.Fatalf("issues = %v, want none", p.Issues)
	}
}

func TestParseProxiesOnlyList(t *testing.T) {
	doc := "- name: N1\n  type: vless\n  server: a.example.com\n  port: 443\n  uuid: u-1\n  tls: true\n" +
		"- name: N2\n  type: hysteria2\n  server: b.example.com\n  port: 443\n  password: pw-2\n"
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Format != FormatProxiesYAML || len(p.Nodes) != 2 {
		t.Fatalf("format=%v nodes=%d", p.Format, len(p.Nodes))
	}
	if p.Nodes[0].Protocol != "vless" || p.Nodes[1].Protocol != "hysteria2" {
		t.Fatalf("protocols = %q %q", p.Nodes[0].Protocol, p.Nodes[1].Protocol)
	}
	if strings.Contains(string(p.SanitizedYAML), "pw-2") {
		t.Fatal("sanitized YAML leaks password")
	}
}

func TestParseProxiesOnlyMapping(t *testing.T) {
	doc := "proxies:\n  - name: N1\n    type: tuic\n    server: a.example.com\n    port: 443\n    uuid: u\n    password: p\n"
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Format != FormatProxiesYAML || len(p.Nodes) != 1 {
		t.Fatalf("format=%v nodes=%d", p.Format, len(p.Nodes))
	}
}

func TestParseYAMLErrors(t *testing.T) {
	cases := map[string]string{
		"unknown protocol": "proxies:\n  - name: A\n    type: snell\n    server: x.com\n    port: 443\n",
		"duplicate node": "proxies:\n  - {name: A, type: ss, server: x.com, port: 1, cipher: c, password: p}\n" +
			"  - {name: A, type: ss, server: y.com, port: 2, cipher: c, password: p}\n",
		"missing server": "proxies:\n  - {name: A, type: ss, port: 1}\n",
		"bad port":       "proxies:\n  - {name: A, type: ss, server: x.com, port: 99999}\n",
		"unknown member": "proxies:\n  - {name: A, type: ss, server: x.com, port: 1, cipher: c, password: p}\n" +
			"proxy-groups:\n  - {name: G, type: select, proxies: [B]}\n",
		"group cycle": "proxy-groups:\n  - {name: G1, type: select, proxies: [G2]}\n  - {name: G2, type: select, proxies: [G1]}\n",
		"node/group name clash": "proxies:\n  - {name: A, type: ss, server: x.com, port: 1, cipher: c, password: p}\n" +
			"proxy-groups:\n  - {name: A, type: select, proxies: [DIRECT]}\n",
		"duplicate key": "proxies: []\nproxies: []\n",
		"unrecognised":  "foo: bar\nbaz: 1\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: Parse succeeded, want error", name)
		}
	}
}

func TestParseSizeLimitAndEmpty(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Fatal("empty input accepted")
	}
	if _, err := Parse(make([]byte, MaxInputBytes+1)); err == nil {
		t.Fatal("oversize input accepted")
	}
}

func vmessURI(t *testing.T, jsonBody string) string {
	t.Helper()
	return "vmess://" + base64.StdEncoding.EncodeToString([]byte(jsonBody))
}

func TestParseURIList(t *testing.T) {
	vmess := vmessURI(t, `{"v":"2","ps":"VM 节点","add":"vm.example.com","port":"443","id":"uuid-1234","aid":"0","net":"ws","type":"none","host":"cdn.example.com","path":"/ws","tls":"tls"}`)
	ssUserinfo := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:ss-pass"))
	lines := strings.Join([]string{
		"# 订阅内容",
		"",
		vmess,
		"vless://uuid-999@vl.example.com:443?security=reality&sni=www.example.com&pbk=PUBKEY&flow=xtls-rprx-vision#VL%E8%8A%82%E7%82%B9",
		"trojan://tj-pass@tj.example.com:443?sni=tj.example.com#TJ",
		"ss://" + ssUserinfo + "@ss.example.com:8388#SS",
		"ss://aes-128-gcm:plain-pass@ss2.example.com:8389#SS2",
		"socks5://user1:sock-pass@sk.example.com:1080#SK",
		"hysteria2://hy-pass@hy.example.com:443?obfs=salamander&obfs-password=obfs-secret&sni=hy.example.com#HY",
		"tuic://tuic-uuid:tuic-pass@tu.example.com:443?congestion_control=bbr#TU",
	}, "\n")
	p, err := Parse([]byte(lines))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Format != FormatURIList || len(p.Nodes) != 8 {
		t.Fatalf("format=%v nodes=%d", p.Format, len(p.Nodes))
	}
	byProto := map[string]Node{}
	for _, n := range p.Nodes {
		byProto[n.Protocol] = n
	}
	if byProto["vmess"].Secrets["uuid"] != "uuid-1234" {
		t.Fatalf("vmess = %+v", byProto["vmess"])
	}
	if byProto["vmess"].Config["network"] != "ws" {
		t.Fatalf("vmess config = %v", byProto["vmess"].Config)
	}
	vl := byProto["vless"]
	if vl.Name != "VL节点" || vl.Secrets["uuid"] != "uuid-999" || vl.Config["flow"] != "xtls-rprx-vision" {
		t.Fatalf("vless = %+v", vl)
	}
	if byProto["trojan"].Secrets["password"] != "tj-pass" {
		t.Fatalf("trojan = %+v", byProto["trojan"])
	}
	byName := map[string]Node{}
	for _, n := range p.Nodes {
		byName[n.Name] = n
	}
	if byName["SS"].Secrets["password"] != "ss-pass" || byName["SS2"].Secrets["password"] != "plain-pass" {
		t.Fatalf("ss nodes wrong: SS=%+v SS2=%+v", byName["SS"], byName["SS2"])
	}
	if byProto["socks5"].Secrets["username"] != "user1" || byProto["socks5"].Secrets["password"] != "sock-pass" {
		t.Fatalf("socks5 = %+v", byProto["socks5"])
	}
	hy := byProto["hysteria2"]
	if hy.Secrets["password"] != "hy-pass" || hy.Secrets["obfs-password"] != "obfs-secret" {
		t.Fatalf("hy2 = %+v", hy)
	}
	if byProto["tuic"].Secrets["uuid"] != "tuic-uuid" || byProto["tuic"].Secrets["password"] != "tuic-pass" {
		t.Fatalf("tuic = %+v", byProto["tuic"])
	}
	san := string(p.SanitizedYAML)
	for _, secret := range []string{"uuid-1234", "ss-pass", "plain-pass", "tj-pass", "sock-pass", "hy-pass", "obfs-secret", "tuic-pass"} {
		if strings.Contains(san, secret) {
			t.Fatalf("sanitized YAML leaks %q", secret)
		}
	}
}

func TestParseSSLegacyAndPlugin(t *testing.T) {
	legacy := "ss://" + base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:legacy-pass@lg.example.com:8390")) + "#LG"
	p, err := Parse([]byte(legacy))
	if err != nil {
		t.Fatalf("legacy Parse: %v", err)
	}
	n := p.Nodes[0]
	if n.Server != "lg.example.com" || n.Port != 8390 || n.Secrets["password"] != "legacy-pass" || n.Config["cipher"] != "chacha20-ietf-poly1305" {
		t.Fatalf("legacy node = %+v", n)
	}
	plugin := "ss://aes-256-gcm:pw@pl.example.com:8388?plugin=obfs-local%3Bobfs%3Dhttp%3Bobfs-host%3Dexample.com#PL"
	p2, err := Parse([]byte(plugin))
	if err != nil {
		t.Fatalf("plugin Parse: %v", err)
	}
	n2 := p2.Nodes[0]
	if n2.Config["plugin"] != "obfs-local" {
		t.Fatalf("plugin config = %v", n2.Config)
	}
	opts, _ := n2.Config["plugin-opts"].(map[string]any)
	if opts["obfs"] != "http" || opts["obfs-host"] != "example.com" {
		t.Fatalf("plugin-opts = %v", opts)
	}
}

func TestParseBase64WrappedURIList(t *testing.T) {
	inner := "ss://aes-256-gcm:pw123@a.example.com:8388#A\nvless://uuid-1@b.example.com:443#B2\n"
	wrapped := base64.StdEncoding.EncodeToString([]byte(inner))
	p, err := Parse([]byte(wrapped))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(p.Nodes))
	}
	// URL-safe, unpadded variant.
	wrapped2 := base64.RawURLEncoding.EncodeToString([]byte(inner))
	if _, err := Parse([]byte(wrapped2)); err != nil {
		t.Fatalf("raw url-safe Parse: %v", err)
	}
}

func TestParseURIErrors(t *testing.T) {
	cases := map[string]string{
		"unknown scheme": "snell://x@y:443#A",
		"bad port":       "vless://uuid@a.com:99999#A",
		"missing uuid":   "vless://@a.com:443#A",
		"bad vmess b64":  "vmess://!!!not-base64!!!",
		"vmess no id":    vmessURI(t, `{"add":"a.com","port":"443"}`),
		"ss no password": "ss://aes-256-gcm@a.com:8388#A",
		"garbage line":   "this is not a uri at all",
		"bad line in ok list": "ss://aes-256-gcm:pw@a.com:8388#A\n" +
			"unknownthing://x@y:1#B",
	}
	for name, input := range cases {
		_, err := Parse([]byte(input))
		if err == nil {
			t.Errorf("%s: Parse succeeded, want error", name)
			continue
		}
		// Errors must never echo secrets or full URIs.
		if strings.Contains(err.Error(), "pw") && name == "bad line in ok list" {
			// scheme name is fine; a password echo is not.
			if strings.Contains(err.Error(), "aes-256-gcm:pw") {
				t.Errorf("%s: error echoes URI content: %v", name, err)
			}
		}
	}
	// Line numbers are reported.
	_, err := Parse([]byte("ss://aes-256-gcm:pw@a.com:8388#A\nsnell://x@y:443#B"))
	if err == nil || !strings.Contains(err.Error(), "第 2 行") {
		t.Fatalf("expected line-numbered error, got %v", err)
	}
}

func TestParseIPv6URI(t *testing.T) {
	p, err := Parse([]byte("vless://uuid-1@[2001:db8::1]:443#V6"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Nodes[0].Server != "2001:db8::1" {
		t.Fatalf("server = %q", p.Nodes[0].Server)
	}
}
