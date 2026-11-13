package mihomoimport

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------
// Stage 6 review fix 1: credential extraction at ANY depth.
// ---------------------------------------------------------------

// TestClassifyFieldsDeepNesting covers the shapes the old
// classifier missed: credentials buried under alternating
// map/array layers, credentials whose value is itself a
// structure, and same-named credentials in different positions
// (their vault paths must not collide).
func TestClassifyFieldsDeepNesting(t *testing.T) {
	in := map[string]any{
		"name": "n1",
		// map -> array -> map -> array -> map
		"outer": map[string]any{
			"list": []any{
				map[string]any{
					"inner": []any{
						map[string]any{"password": "deep-secret-1", "keep": "v1"},
					},
				},
			},
		},
		// array -> array -> map
		"matrix": []any{
			[]any{
				map[string]any{"uuid": "deep-secret-2"},
			},
		},
		// A credential whose value is a whole structure: the
		// structure must be preserved in the vault, not flattened
		// away or left behind.
		"auth":  map[string]any{"user": "u", "pass": "p"},
		"token": []any{"t1", "t2"},
		// Same key name in several positions: distinct paths.
		"a": map[string]any{"password": "same-name-A"},
		"b": []any{map[string]any{"password": "same-name-B0"}, map[string]any{"password": "same-name-B1"}},
		// Non-secret content that must survive untouched.
		"plain":  42,
		"nested": map[string]any{"arr": []any{1, "two", true, map[string]any{"x": "y"}}},
	}
	config, secrets := classifyFields(in)

	wantPaths := map[string]string{
		"outer.list.0.inner.0.password": "deep-secret-1",
		"matrix.0.0.uuid":               "deep-secret-2",
		"a.password":                    "same-name-A",
		"b.0.password":                  "same-name-B0",
		"b.1.password":                  "same-name-B1",
	}
	for path, want := range wantPaths {
		if secrets[path] != want {
			t.Errorf("secrets[%q] = %q, want %q (all: %v)", path, secrets[path], want, secrets)
		}
	}
	// Structured credential values survive as JSON in the vault.
	var authVal map[string]any
	if err := json.Unmarshal([]byte(secrets["auth"]), &authVal); err != nil || authVal["pass"] != "p" {
		t.Errorf("auth secret = %q, want JSON of the structure", secrets["auth"])
	}
	if secrets["token"] != `["t1","t2"]` {
		t.Errorf("token secret = %q, want the JSON array", secrets["token"])
	}
	// The cleaned config contains NO secret at any depth.
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"deep-secret-1", "deep-secret-2", "same-name-A", "same-name-B0", "same-name-B1", `"pass":"p"`, "t1"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("cleaned config leaks %q: %s", s, raw)
		}
	}
	for _, key := range []string{"password", "uuid", "auth", "token"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("cleaned config still has key %q: %s", key, raw)
		}
	}
	// Non-secret data is complete and unchanged in shape.
	if config["plain"] != 42 || config["name"] != "n1" {
		t.Errorf("scalars damaged: %v", config)
	}
	nested, _ := config["nested"].(map[string]any)
	arr, _ := nested["arr"].([]any)
	if len(arr) != 4 || arr[3].(map[string]any)["x"] != "y" {
		t.Errorf("non-secret nesting damaged: %v", config["nested"])
	}
	outer, _ := config["outer"].(map[string]any)
	list, _ := outer["list"].([]any)
	innerMap, _ := list[0].(map[string]any)["inner"].([]any)[0].(map[string]any)
	if innerMap["keep"] != "v1" {
		t.Errorf("non-secret sibling of a deep secret lost: %v", innerMap)
	}
	// Input was not mutated.
	origOuter := in["outer"].(map[string]any)["list"].([]any)[0].(map[string]any)["inner"].([]any)[0].(map[string]any)
	if origOuter["password"] != "deep-secret-1" {
		t.Error("classifyFields mutated its input")
	}
}

// TestParseYAMLNestedArraySecrets drives the same shapes through
// the full YAML pipeline: Node.Config (which lands in
// proxies.config_json) and the persisted SanitizedYAML must both
// be clean, with every secret in Node.Secrets.
func TestParseYAMLNestedArraySecrets(t *testing.T) {
	doc := `
proxies:
  - name: 嵌套节点
    type: ss
    server: n.example.com
    port: 8388
    cipher: aes-256-gcm
    password: top-secret
    extra:
      layers:
        - - name: l1
            password: nested-secret-array
        - - uuid: nested-secret-uuid
      endpoints:
        - host: e1.example.com
          auth:
            password: nested-secret-auth
    fallbacks:
      - - token: nested-secret-token
`
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	node := p.Nodes[0]
	rawCfg, err := json.Marshal(node.Config)
	if err != nil {
		t.Fatal(err)
	}
	allSecrets := []string{"top-secret", "nested-secret-array", "nested-secret-uuid", "nested-secret-auth", "nested-secret-token"}
	joinedSecrets := ""
	for _, v := range node.Secrets {
		joinedSecrets += v + "\n"
	}
	for _, s := range allSecrets {
		if strings.Contains(string(rawCfg), s) {
			t.Errorf("Node.Config leaks %q: %s", s, rawCfg)
		}
		if strings.Contains(string(p.SanitizedYAML), s) {
			t.Errorf("SanitizedYAML leaks %q", s)
		}
		if !strings.Contains(joinedSecrets, s) {
			t.Errorf("secret %q missing from Node.Secrets: %v", s, node.Secrets)
		}
	}
	// The dotted paths distinguish the nested positions. Note the
	// "auth" case: auth is itself a credential key, so the whole
	// structure is vaulted as one entry at the auth path (its JSON
	// form still carries the inner password, checked above).
	for _, path := range []string{
		"extra.layers.0.0.password",
		"extra.layers.1.0.uuid",
		"extra.endpoints.0.auth",
		"fallbacks.0.0.token",
	} {
		if _, ok := node.Secrets[path]; !ok {
			t.Errorf("missing secret path %q in %v", path, node.Secrets)
		}
	}
	// Non-secret neighbours survive in the config.
	if !strings.Contains(string(rawCfg), "e1.example.com") || !strings.Contains(string(rawCfg), "l1") {
		t.Errorf("non-secret nested data lost: %s", rawCfg)
	}
}

// TestParseURINestedConfigSecrets: URI imports run through the
// same classifier — a plugin-opts password inside an ss URI must
// be extracted, not stored in the node config.
func TestParseURINestedSecrets(t *testing.T) {
	uri := "ss://aes-256-gcm:pw@a.example.com:8388?plugin=v2ray-plugin%3Bpassword%3Dplugin-secret#A"
	p, err := Parse([]byte(uri))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rawCfg, _ := json.Marshal(p.Nodes[0].Config)
	if strings.Contains(string(rawCfg), "plugin-secret") {
		t.Fatalf("URI node config leaks plugin password: %s", rawCfg)
	}
	if p.Nodes[0].Secrets["plugin-opts.password"] != "plugin-secret" {
		t.Fatalf("secrets = %v", p.Nodes[0].Secrets)
	}
}

// ---------------------------------------------------------------
// Stage 6 review fix 2: YAML aliases are rejected.
// ---------------------------------------------------------------

func TestParseRejectsAliases(t *testing.T) {
	secret := "alias-secret-value"
	cases := map[string]string{
		"plain alias": `
base: &base
  type: ss
  server: a.example.com
  port: 8388
  cipher: aes-256-gcm
  password: ` + secret + `
proxies:
  - <<: *base
    name: A
`,
		"alias of node": `
shared: &node
  name: A
  type: ss
  server: a.example.com
  port: 8388
  cipher: c
  password: ` + secret + `
proxies:
  - *node
`,
		"nested alias in list": `
items: &items
  - one
proxies:
  - name: A
    type: ss
    server: a.example.com
    port: 8388
    cipher: c
    password: ` + secret + `
    extra: *items
`,
		"multi-level alias structure": `
a: &a ["x", "y"]
b: &b [*a, *a]
c: &c [*b, *b]
proxies:
  - name: A
    type: ss
    server: a.example.com
    port: 8388
    cipher: c
    password: ` + secret + `
    extra: *c
`,
		"alias bomb shape": `
a: &a ["lol", "lol", "lol", "lol", "lol", "lol", "lol", "lol", "lol"]
b: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a]
c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b]
d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c]
proxies:
  - name: A
    type: ss
    server: a.example.com
    port: 8388
    cipher: c
    password: ` + secret + `
    extra: *d
`,
	}
	for name, doc := range cases {
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Errorf("%s: Parse succeeded, want alias rejection", name)
			continue
		}
		if !strings.Contains(err.Error(), "alias") {
			t.Errorf("%s: error does not name the alias problem: %v", name, err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the document secret: %v", name, err)
		}
	}
}

// TestParseAnchorWithoutAliasStillWorks: an anchor that is never
// referenced by an alias creates no AliasNode and stays legal.
func TestParseAnchorWithoutAliasStillWorks(t *testing.T) {
	doc := `
proxies:
  - &only
    name: A
    type: ss
    server: a.example.com
    port: 8388
    cipher: aes-256-gcm
    password: pw
`
	p, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Nodes) != 1 || p.Nodes[0].Secrets["password"] != "pw" {
		t.Fatalf("nodes = %+v", p.Nodes)
	}
}
