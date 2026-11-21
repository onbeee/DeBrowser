// Package mihomo integrates the official Mihomo proxy core into
// DeBrowser: it generates the core's runtime configuration from
// DeBrowser's own business data (sources + instance bindings),
// talks to the core's REST API, and manages the core process.
//
// DeBrowser never reimplements proxy behaviour: nodes, rules, DNS
// and connections are all the core's job. The generated
// configuration contains no node definitions at all — nodes arrive
// through the core's own proxy-provider mechanism.
package mihomo

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SourceInput is one proxy-provider of the generated
// configuration. URL is the subscription address (URL sources
// only): it is read from the credential vault at generation time,
// written only into the generated configuration file inside the
// core's working directory, and never logged. FileName is the
// basename of DeBrowser's copy of an imported local YAML file
// (file sources only), relative to <workdir>/sources/.
type SourceInput struct {
	ProviderName string
	Kind         string // "url" | "file"
	URL          string
	FileName     string
}

// BindingInput is one browser instance's proxy binding as the
// generated configuration needs it: which provider its node comes
// from, the node's name inside that provider, the routing mode and
// the loopback listener port assigned to the instance.
type BindingInput struct {
	InstanceID   string
	ProviderName string
	NodeName     string
	Mode         string // "rule" | "global"
	Port         int
}

// ConfigInput is everything Generate needs.
type ConfigInput struct {
	ControllerPort int
	Secret         string
	Sources        []SourceInput
	Bindings       []BindingInput
}

// shortID derives a stable, configuration-safe name fragment from
// a UUID: its first 12 hex digits.
func shortID(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) > 12 {
		s = s[:12]
	}
	return s
}

// GroupName is the per-instance proxy select group name.
func GroupName(instanceID string) string { return "inst-" + shortID(instanceID) }

// DelayGroupName is the hidden per-source select group used only
// for node delay tests (it exists even when no instance is bound
// to the source).
func DelayGroupName(providerName string) string { return "delay-" + providerName }

// DefaultSubRules returns DeBrowser's default rule lines for one
// instance, with the proxy target rewritten to the instance's own
// group. The lines are deliberately few, local and offline-safe
// (no GEOIP / remote rule providers): loopback and private ranges
// go direct, everything else goes to the instance's selected node.
// Rule order is significant and preserved.
func DefaultSubRules(group string) []string {
	return []string{
		"DOMAIN-SUFFIX,localhost,DIRECT",
		"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
		"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
		"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
		"MATCH," + group,
	}
}

// GlobalSubRules is the per-instance global mode: a single MATCH
// line to the instance's own group. The core's top-level mode
// always stays "rule"; global is per instance only.
func GlobalSubRules(group string) []string {
	return []string{"MATCH," + group}
}

// yq quotes a string as a YAML scalar. A JSON double-quoted string
// is a valid YAML scalar and escapes everything that could break
// out of the value (quotes, backslashes, control characters), so
// subscription URLs and node names stay inert data. HTML escaping
// is off so URLs stay readable in the generated file (& stays &);
// the core decodes the value identically either way.
func yq(s string) string {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSpace(buf.String())
}

// Generate renders the complete Mihomo runtime configuration.
// The output is deterministic (sources and bindings are sorted by
// name/port) so tests can compare it byte for byte.
func Generate(in ConfigInput) (string, error) {
	if in.ControllerPort <= 0 || in.ControllerPort > 65535 {
		return "", fmt.Errorf("mihomo: controller port %d out of range", in.ControllerPort)
	}
	if in.Secret == "" {
		return "", fmt.Errorf("mihomo: controller secret is empty")
	}
	sources := append([]SourceInput(nil), in.Sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ProviderName < sources[j].ProviderName })
	bindings := append([]BindingInput(nil), in.Bindings...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].Port < bindings[j].Port })
	for _, b := range bindings {
		if b.Port <= 0 || b.Port > 65535 {
			return "", fmt.Errorf("mihomo: listener port %d out of range", b.Port)
		}
		if b.Mode != "rule" && b.Mode != "global" {
			return "", fmt.Errorf("mihomo: binding mode %q invalid", b.Mode)
		}
	}

	var w strings.Builder
	w.WriteString("# DeBrowser 生成的 Mihomo 运行配置，请勿手动编辑。\n")
	w.WriteString("# 订阅地址等敏感信息只出现在本文件中，不要分享或导出本文件。\n")
	w.WriteString("log-level: warning\n")
	w.WriteString("mode: rule\n")
	w.WriteString("ipv6: false\n")
	fmt.Fprintf(&w, "external-controller: %s\n", yq(fmt.Sprintf("127.0.0.1:%d", in.ControllerPort)))
	fmt.Fprintf(&w, "secret: %s\n", yq(in.Secret))
	// DeBrowser owns instance->node selections in its database;
	// the core must not persist its own copy of group selections.
	w.WriteString("profile:\n  store-selected: false\n")
	// DNS is the core's job. redir-host keeps name resolution
	// semantics unsurprising for browsers that hand hostnames to
	// the proxy; the system resolver is the default upstream.
	w.WriteString("dns:\n  enable: true\n  ipv6: false\n  enhanced-mode: redir-host\n  nameserver:\n    - system\n")

	if len(sources) > 0 {
		w.WriteString("proxy-providers:\n")
		for _, s := range sources {
			fmt.Fprintf(&w, "  %s:\n", s.ProviderName)
			switch s.Kind {
			case "url":
				w.WriteString("    type: http\n")
				fmt.Fprintf(&w, "    url: %s\n", yq(s.URL))
				w.WriteString("    interval: 86400\n")
				// The provider's own fetch must not traverse the
				// rule engine (it would depend on the very nodes it
				// is fetching); pin it to a direct connection.
				w.WriteString("    proxy: DIRECT\n")
				fmt.Fprintf(&w, "    path: %s\n", yq("./providers/"+s.ProviderName+".yaml"))
			case "file":
				w.WriteString("    type: file\n")
				fmt.Fprintf(&w, "    path: %s\n", yq("./sources/"+s.FileName))
			default:
				return "", fmt.Errorf("mihomo: source kind %q invalid", s.Kind)
			}
			w.WriteString("    health-check:\n      enable: true\n      lazy: true\n")
			w.WriteString("      url: \"https://www.gstatic.com/generate_204\"\n      interval: 300\n")
		}
	}

	if len(sources) > 0 || len(bindings) > 0 {
		w.WriteString("proxy-groups:\n")
		for _, s := range sources {
			// One hidden delay-test group per source: provider
			// nodes are not individually addressable for delay
			// tests, and per-source groups keep same-named nodes
			// of different sources from ever sharing a namespace.
			fmt.Fprintf(&w, "  - name: %s\n    type: select\n    hidden: true\n    use:\n      - %s\n",
				yq(DelayGroupName(s.ProviderName)), s.ProviderName)
		}
		providerSet := map[string]bool{}
		for _, s := range sources {
			providerSet[s.ProviderName] = true
		}
		for _, b := range bindings {
			if !providerSet[b.ProviderName] {
				return "", fmt.Errorf("mihomo: binding for instance %s references unknown provider %q", b.InstanceID, b.ProviderName)
			}
			// Each instance group draws nodes from its bound
			// source's provider only: same-named nodes in other
			// sources can never leak into this instance.
			fmt.Fprintf(&w, "  - name: %s\n    type: select\n    use:\n      - %s\n",
				yq(GroupName(b.InstanceID)), b.ProviderName)
		}
	}

	if len(bindings) > 0 {
		w.WriteString("listeners:\n")
		for _, b := range bindings {
			name := GroupName(b.InstanceID)
			fmt.Fprintf(&w, "  - name: %s\n    type: mixed\n    listen: \"127.0.0.1\"\n    port: %d\n    rule: %s\n",
				yq(name), b.Port, name)
		}
		w.WriteString("sub-rules:\n")
		for _, b := range bindings {
			name := GroupName(b.InstanceID)
			lines := DefaultSubRules(name)
			if b.Mode == "global" {
				lines = GlobalSubRules(name)
			}
			fmt.Fprintf(&w, "  %s:\n", name)
			for _, ln := range lines {
				fmt.Fprintf(&w, "    - %s\n", yq(ln))
			}
		}
	}

	// The top-level rule list serves only the core's own traffic
	// (provider fetches are pinned to DIRECT on the provider
	// itself); instance traffic always enters through listeners
	// and their sub-rules.
	w.WriteString("rules:\n  - \"MATCH,DIRECT\"\n")
	return w.String(), nil
}
