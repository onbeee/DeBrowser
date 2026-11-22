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
// from, the node's name inside that provider and the loopback
// listener port assigned to the instance. RouteMode is the
// binding's recorded routing mode ("rule" | "global" | "direct";
// empty means "rule"): it decides the instance's sub-rules.
type BindingInput struct {
	InstanceID   string
	ProviderName string
	NodeName     string
	Port         int
	RouteMode    string
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

// SubRuleName is the per-instance sub-rules set name: the
// instance listener's `rule` target. Its final MATCH line lands
// on the instance's own group (modes rule/global) or on DIRECT
// (mode direct), so instances share the rule-providers but never
// each other's routing decision.
func SubRuleName(instanceID string) string { return "sub-" + GroupName(instanceID) }

// ruleProviderDef is one shared rule-provider of the generated
// configuration. The list is fixed by the product: every
// rule-mode instance routes over the same providers (the core
// downloads and caches them once, under ./rules/), and the
// per-instance sub-rules differ only in which group their
// proxy-bound sets and final MATCH point at.
type ruleProviderDef struct {
	Name string // provider name, referenced by RULE-SET lines
	URL  string
	File string // cache file basename under ./rules/
}

var sharedRuleProviders = []ruleProviderDef{
	{"规则修正", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Direct/Direct.yaml", "direct"},
	{"屏蔽服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/BlockHttpDNS/BlockHttpDNS.yaml", "block-http-dns"},
	{"禁止广告", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/AdvertisingLite/AdvertisingLite_Classical.yaml", "advertising-lite"},
	{"反劫服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Hijacking/Hijacking.yaml", "hijacking"},
	{"隐私保护", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Privacy/Privacy_Classical.yaml", "privacy"},
	{"智能服务", "https://raw.githubusercontent.com/KPP69/CF/main/Clash/Rule/Ai.yaml", "ai"},
	{"谷歌服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Google/Google.yaml", "google"},
	{"推特服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Twitter/Twitter.yaml", "twitter"},
	{"飞机服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Telegram/Telegram.yaml", "telegram"},
	{"加密服务", "https://raw.githubusercontent.com/KPP69/CF/main/Clash/Rule/Crypto.yaml", "crypto"},
	{"微软服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Microsoft/Microsoft.yaml", "microsoft"},
	{"国际服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Proxy/Proxy_Classical.yaml", "proxy"},
	{"国内服务", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/ChinaMax/ChinaMax_Classical.yaml", "china-max"},
	{"局域网络", "https://raw.githubusercontent.com/blackmatrix7/ios_rule_script/master/rule/Clash/Lan/Lan.yaml", "lan"},
}

// subRuleLines renders one instance's sub-rules from its routing
// mode. In rule mode the shared sets run in their fixed order —
// direct-fixes first, block sets next, the instance's group for
// everything proxy-bound, China/LAN direct — and the final MATCH
// lands on the instance's own group, so a request no set claims
// still uses exactly the instance's bound node. Mode values are
// model.RouteMode* (the database CHECK constrains them).
func subRuleLines(b BindingInput) []string {
	group := GroupName(b.InstanceID)
	switch b.RouteMode {
	case "global":
		return []string{"MATCH," + group}
	case "direct":
		return []string{"MATCH,DIRECT"}
	default: // "rule" (and the zero value)
		return []string{
			"RULE-SET,规则修正,DIRECT",
			"RULE-SET,屏蔽服务,REJECT",
			"RULE-SET,禁止广告,REJECT",
			"RULE-SET,反劫服务,REJECT",
			"RULE-SET,隐私保护,REJECT",
			"RULE-SET,智能服务," + group,
			"RULE-SET,谷歌服务," + group,
			"RULE-SET,推特服务," + group,
			"RULE-SET,飞机服务," + group,
			"RULE-SET,加密服务," + group,
			"RULE-SET,微软服务," + group,
			"RULE-SET,国际服务," + group,
			"RULE-SET,国内服务,DIRECT",
			"RULE-SET,局域网络,DIRECT",
			"GEOIP,CN,DIRECT,no-resolve",
			"MATCH," + group,
		}
	}
}

// DelayGroupName is the hidden per-source select group used only
// for node delay tests (it exists even when no instance is bound
// to the source).
func DelayGroupName(providerName string) string { return "delay-" + providerName }

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
		switch b.RouteMode {
		case "", "rule", "global", "direct":
		default:
			return "", fmt.Errorf("mihomo: binding for instance %s has invalid route mode %q", b.InstanceID, b.RouteMode)
		}
	}

	var w strings.Builder
	w.WriteString("# DeBrowser 生成的 Mihomo 运行配置，请勿手动编辑。\n")
	w.WriteString("# 订阅地址等敏感信息只出现在本文件中，不要分享或导出本文件。\n")
	w.WriteString("log-level: warning\n")
	w.WriteString("mode: rule\n")
	w.WriteString("ipv6: false\n")
	// tcp-concurrent races a hostname's addresses instead of
	// dialing them one by one, so one dead address cannot stall a
	// connection (or a delay test) until timeout. unified-delay
	// drops the extra handshake round trip from delay measurement,
	// keeping measured delays comparable with other Mihomo
	// clients.
	w.WriteString("tcp-concurrent: true\n")
	w.WriteString("unified-delay: true\n")
	fmt.Fprintf(&w, "external-controller: %s\n", yq(fmt.Sprintf("127.0.0.1:%d", in.ControllerPort)))
	fmt.Fprintf(&w, "secret: %s\n", yq(in.Secret))
	// DeBrowser owns instance->node selections in its database;
	// the core must not persist its own copy of group selections.
	w.WriteString("profile:\n  store-selected: false\n")
	// DNS is the core's job. redir-host keeps name resolution
	// semantics unsurprising for browsers that hand hostnames to
	// the proxy. The upstreams are DoH endpoints, NOT the system
	// resolver: under redir-host the system resolver can stall
	// the first query for a name badly enough to fail concurrent
	// delay tests (MetaCubeX/mihomo#1185). default-nameserver
	// resolves the DoH servers' own hostnames (plain IPs here,
	// so bootstrap needs no resolution); proxy-server-nameserver
	// resolves proxy node hostnames, so node dialing never
	// depends on the general nameserver path.
	w.WriteString("dns:\n  enable: true\n  ipv6: false\n  enhanced-mode: redir-host\n")
	w.WriteString("  default-nameserver:\n    - \"223.5.5.5\"\n    - \"223.6.6.6\"\n")
	w.WriteString("  proxy-server-nameserver:\n    - \"https://223.5.5.5/dns-query\"\n    - \"https://223.6.6.6/dns-query\"\n")
	w.WriteString("  nameserver:\n    - \"https://223.5.5.5/dns-query\"\n    - \"https://223.6.6.6/dns-query\"\n")

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
			// sources can never leak into this instance. REJECT is
			// the first member on purpose: a select group falls
			// back to its first member when the selected node
			// disappears, so the default state refuses traffic
			// instead of silently switching to another node.
			// DeBrowser sets the bound node explicitly after every
			// start/reload (store-selected stays false).
			fmt.Fprintf(&w, "  - name: %s\n    type: select\n    proxies:\n      - REJECT\n    use:\n      - %s\n",
				yq(GroupName(b.InstanceID)), b.ProviderName)
		}
	}

	if len(bindings) > 0 {
		w.WriteString("listeners:\n")
		for _, b := range bindings {
			name := GroupName(b.InstanceID)
			// The listener hands its traffic to the instance's
			// own sub-rules (never straight to a group): the
			// sub-rules are what makes the routing mode real —
			// shared rule sets for mode rule, a single MATCH to
			// the instance's group for mode global, a single
			// MATCH,DIRECT for mode direct.
			fmt.Fprintf(&w, "  - name: %s\n    type: mixed\n    listen: \"127.0.0.1\"\n    port: %d\n    rule: %s\n",
				yq(name), b.Port, yq(SubRuleName(b.InstanceID)))
		}
	}

	anyRuleMode := false
	for _, b := range bindings {
		if b.RouteMode == "" || b.RouteMode == "rule" {
			anyRuleMode = true
			break
		}
	}
	if anyRuleMode {
		// The shared rule-providers exist only when at least one
		// instance can reference them; the core downloads and
		// caches them once for all rule-mode instances.
		w.WriteString("rule-providers:\n")
		for _, rp := range sharedRuleProviders {
			fmt.Fprintf(&w, "  %s:\n    type: http\n    behavior: classical\n    format: yaml\n    url: %s\n    path: %s\n    interval: 86400\n",
				yq(rp.Name), yq(rp.URL), yq("./rules/"+rp.File+".yaml"))
		}
	}

	if len(bindings) > 0 {
		w.WriteString("sub-rules:\n")
		for _, b := range bindings {
			fmt.Fprintf(&w, "  %s:\n", yq(SubRuleName(b.InstanceID)))
			for _, line := range subRuleLines(b) {
				fmt.Fprintf(&w, "    - %s\n", yq(line))
			}
		}
	}

	// The top-level rule list serves only the core's own traffic
	// (provider fetches are pinned to DIRECT on the provider
	// itself); instance traffic enters through the listeners and
	// never consults this list.
	w.WriteString("rules:\n  - \"MATCH,DIRECT\"\n")
	return w.String(), nil
}
