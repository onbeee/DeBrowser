package mihomo

// Configuration generation for the proxy core: a pure function
// from the database state to the core's config file. The shape
// is deliberately the smallest one that gives every bound
// instance its own node:
//
//   - every imported node appears once under `proxies:`, under
//     a core-side name derived from its database id (so same-
//     named nodes from different sources never collide);
//   - every binding becomes one loopback `listeners:` entry
//     whose `proxy:` names exactly the bound node — the core's
//     native per-listener proxy selection, verified against
//     this core version. A listener bound this way sends all
//     of its traffic through that one node.
//
// There are no rules, no rule providers, no proxy groups and
// no DNS overrides: nothing else is needed for the feature,
// and the core's own defaults apply everywhere else.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func jsonCompact(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(data)
}

// NodeDef is one node for the generated configuration: its
// core-side name and its full definition (as parsed at import
// time, with the display name still inside — Generate replaces
// the name with the core-side one).
type NodeDef struct {
	CoreName string
	Config   map[string]any
}

// BindingDef is one instance binding for the generated
// configuration: the listener's port and the core-side name of
// the node it must use.
type BindingDef struct {
	Port         int
	NodeCoreName string
}

// Generate renders the complete core configuration. Output is
// deterministic: nodes sort by core name, listeners by port.
func Generate(nodes []NodeDef, bindings []BindingDef, controllerPort int, secret string) string {
	var b strings.Builder
	b.WriteString("# DeBrowser 生成的 Mihomo 配置：请勿手动编辑，重新生成会覆盖。\n")
	fmt.Fprintf(&b, "external-controller: 127.0.0.1:%d\n", controllerPort)
	fmt.Fprintf(&b, "secret: %s\n", yamlScalar(secret))
	b.WriteString("log-level: warning\n")

	sortedNodes := append([]NodeDef(nil), nodes...)
	sort.Slice(sortedNodes, func(i, j int) bool { return sortedNodes[i].CoreName < sortedNodes[j].CoreName })
	if len(sortedNodes) == 0 {
		b.WriteString("proxies: []\n")
	} else {
		b.WriteString("proxies:\n")
		for _, n := range sortedNodes {
			cfg := make(map[string]any, len(n.Config)+1)
			for k, v := range n.Config {
				cfg[k] = v
			}
			cfg["name"] = n.CoreName
			b.WriteString("  - ")
			b.WriteString(yamlFlowMap(cfg))
			b.WriteString("\n")
		}
	}

	sortedBindings := append([]BindingDef(nil), bindings...)
	sort.Slice(sortedBindings, func(i, j int) bool { return sortedBindings[i].Port < sortedBindings[j].Port })
	if len(sortedBindings) == 0 {
		b.WriteString("listeners: []\n")
	} else {
		b.WriteString("listeners:\n")
		for _, bd := range sortedBindings {
			fmt.Fprintf(&b, "  - name: %s\n", yamlScalar("inst-"+fmt.Sprint(bd.Port)))
			b.WriteString("    type: mixed\n")
			b.WriteString("    listen: 127.0.0.1\n")
			fmt.Fprintf(&b, "    port: %d\n", bd.Port)
			fmt.Fprintf(&b, "    proxy: %s\n", yamlScalar(bd.NodeCoreName))
		}
	}
	return b.String()
}

// yamlScalar renders a string as a YAML scalar using JSON
// double-quote escaping, which is valid YAML and unambiguous
// for arbitrary names.
func yamlScalar(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlFlowMap renders a node definition as a one-line YAML flow
// mapping with sorted keys. Node definitions are flat scalar
// maps in practice (name/type/server/port and option scalars);
// nested values (rare plugin options) are rendered as JSON,
// which YAML flow style accepts.
func yamlFlowMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, yamlScalar(k)+": "+yamlValue(m[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func yamlValue(v any) string {
	switch t := v.(type) {
	case string:
		return yamlScalar(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprint(t)
	case int64:
		return fmt.Sprint(t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprint(int64(t))
		}
		return fmt.Sprint(t)
	case nil:
		return "null"
	default:
		// Nested maps/slices: JSON is valid YAML flow style.
		return jsonCompact(v)
	}
}
