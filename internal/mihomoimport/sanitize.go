package mihomoimport

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// YAML structural limits (Stage 6 instruction: bound node count
// and nesting depth so a hostile document cannot exhaust memory).
const (
	maxYAMLNodes = 100_000
	maxYAMLDepth = 100
)

// checkYAMLTree walks a parsed document enforcing the structural
// limits and rejecting duplicate mapping keys anywhere.
func checkYAMLTree(root *yaml.Node) error {
	count := 0
	var walk func(n *yaml.Node, depth int) error
	walk = func(n *yaml.Node, depth int) error {
		if n == nil {
			return nil
		}
		count++
		if count > maxYAMLNodes {
			return errf("YAML 节点数量超过上限")
		}
		if depth > maxYAMLDepth {
			return errf("YAML 嵌套层级超过上限")
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				if err := walk(c, depth+1); err != nil {
					return err
				}
			}
		case yaml.MappingNode:
			seen := map[string]bool{}
			for i := 0; i+1 < len(n.Content); i += 2 {
				k := n.Content[i].Value
				if seen[k] {
					return errf("YAML 存在重复的映射键 %q", k)
				}
				seen[k] = true
				if err := walk(n.Content[i], depth+1); err != nil {
					return err
				}
				if err := walk(n.Content[i+1], depth+1); err != nil {
					return err
				}
			}
		case yaml.SequenceNode:
			for _, c := range n.Content {
				if err := walk(c, depth+1); err != nil {
					return err
				}
			}
		case yaml.AliasNode:
			// Aliases are followed for limit purposes only; the
			// target was already counted at its anchor site, so do
			// not recurse (an alias chain could otherwise loop).
		}
		return nil
	}
	return walk(root, 0)
}

// nodeToAny converts a yaml.Node subtree into plain Go values
// (map[string]any / []any / string / int / float64 / bool / nil),
// resolving aliases. Duplicate keys were already rejected by
// checkYAMLTree.
func nodeToAny(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return nodeToAny(n.Content[0])
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			v, err := nodeToAny(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[n.Content[i].Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := nodeToAny(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, errf("YAML 别名无法解析")
		}
		return nodeToAny(n.Alias)
	case yaml.ScalarNode:
		return scalarValue(n), nil
	}
	return nil, errf("不支持的 YAML 节点类型")
}

// scalarValue decodes a scalar node according to its tag.
func scalarValue(n *yaml.Node) any {
	switch n.Tag {
	case "!!int":
		if v, err := strconv.ParseInt(strings.ReplaceAll(n.Value, "_", ""), 0, 64); err == nil {
			return int(v)
		}
	case "!!float":
		if v, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64); err == nil {
			return v
		}
	case "!!bool":
		return n.Value == "true" || n.Value == "True" || n.Value == "TRUE"
	case "!!null":
		return nil
	}
	return n.Value
}

// scalarString renders any scalar-ish value as a string for
// credential storage (credentials are opaque strings in the
// vault envelope).
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// classifyFields splits a decoded mapping into non-secret config
// and extracted secrets. Secret keys are recognised at ANY depth;
// nested secrets are recorded under dotted paths (for example
// "plugin-opts.password") and removed from the config copy. The
// input map is not modified.
func classifyFields(in map[string]any) (config map[string]any, secrets map[string]string) {
	config = make(map[string]any, len(in))
	secrets = map[string]string{}
	for key, val := range in {
		classifyValue(key, key, val, config, secrets)
	}
	return config, secrets
}

// classifyValue handles one key/value pair. When the key itself
// is a credential name, the whole value becomes one secret
// (scalars in natural form, containers as compact JSON).
func classifyValue(key, path string, val any, config map[string]any, secrets map[string]string) {
	if isSecretKey(key) {
		secrets[path] = secretString(val)
		return
	}
	switch child := val.(type) {
	case map[string]any:
		sub := make(map[string]any, len(child))
		for ck, cv := range child {
			classifyValue(ck, path+"."+ck, cv, sub, secrets)
		}
		config[key] = sub
	case []any:
		out := make([]any, len(child))
		for i, cv := range child {
			if m, ok := cv.(map[string]any); ok {
				sub := make(map[string]any, len(m))
				for ck, cvv := range m {
					classifyValue(ck, fmt.Sprintf("%s.%d.%s", path, i, ck), cvv, sub, secrets)
				}
				out[i] = sub
			} else {
				out[i] = cv
			}
		}
		config[key] = out
	default:
		config[key] = val
	}
}

// secretString renders an extracted credential value as the
// string stored in the vault. Scalars use their natural form;
// containers are encoded as compact JSON so nothing is lost.
func secretString(v any) string {
	switch v.(type) {
	case map[string]any, []any:
		b, err := marshalJSON(v)
		if err != nil {
			return ""
		}
		return string(b)
	default:
		return scalarString(v)
	}
}

// scrubUserinfo removes the userinfo credential part from a URL
// string value, returning the cleaned URL and the extracted
// userinfo ("" when the value is not a URL or carries none). The
// host, path and query survive — only the credential is vaulted.
func scrubUserinfo(s string) (cleaned string, userinfo string) {
	if !strings.Contains(s, "://") {
		return s, ""
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s, ""
	}
	userinfo = u.User.String()
	u.User = url.User(Placeholder)
	return u.String(), userinfo
}

// extractConfigSecrets walks a YAML document tree IN PLACE: every
// credential-named field and every URL userinfo is replaced by
// the placeholder and recorded in bundle under its document path
// (dot-separated keys, numeric sequence indices). Paths listed in
// skipPrefixes (the proxies / proxy-groups sections, whose
// credentials are extracted per node/group beforehand) are not
// visited.
func extractConfigSecrets(doc *yaml.Node, bundle map[string]string) {
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				keyNode, valNode := n.Content[i], n.Content[i+1]
				key := keyNode.Value
				childPath := key
				if path != "" {
					childPath = path + "." + key
				}
				if skippedPath(childPath) {
					continue
				}
				if isSecretKey(key) {
					if valNode.Kind == yaml.ScalarNode && valNode.Value == Placeholder {
						continue // already extracted in an earlier pass
					}
					raw, err := nodeToAny(valNode)
					if err != nil {
						continue
					}
					if s := secretString(raw); s != "" {
						bundle[childPath] = s
					}
					replaceScalar(valNode)
					continue
				}
				walk(valNode, childPath)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(c, fmt.Sprintf("%s.%d", path, i))
			}
		case yaml.ScalarNode:
			if cleaned, ui := scrubUserinfo(n.Value); ui != "" {
				bundle[path+"#userinfo"] = ui
				n.Value = cleaned
			}
		case yaml.AliasNode:
			// Aliases share their target; extracting through an
			// alias would double-record. Targets are visited at
			// their anchor site.
		}
	}
	walk(doc, "")
}

func skippedPath(p string) bool {
	return p == "proxies" || strings.HasPrefix(p, "proxies.") ||
		p == "proxy-groups" || strings.HasPrefix(p, "proxy-groups.")
}

// replaceScalar turns a node into the placeholder scalar (used
// where a whole subtree was extracted as one credential).
func replaceScalar(n *yaml.Node) {
	n.Kind = yaml.ScalarNode
	n.Tag = "!!str"
	n.Value = Placeholder
	n.Content = nil
	n.Alias = nil
}

// securityReport inspects the ORIGINAL decoded document (before
// sanitisation) and records the Stage 6 findings. Values reported
// are listener addresses only — never credential material.
func securityReport(root map[string]any) []SecurityIssue {
	var issues []SecurityIssue
	add := func(code, msg string) {
		issues = append(issues, SecurityIssue{Code: code, Message: msg})
	}
	if root == nil {
		return issues
	}
	if v, ok := root["allow-lan"].(bool); ok && v {
		add("allow_lan", "原始配置启用了 allow-lan（允许局域网访问），Stage 7 渲染时将默认关闭")
	}
	if v, ok := root["bind-address"].(string); ok && v != "" && !isLocalhostHost(v) {
		add("external_bind", "原始配置的 bind-address 不是本地地址，Stage 7 渲染时将强制使用 127.0.0.1")
	}
	if v, ok := root["external-controller"].(string); ok && v != "" {
		host := v
		if h, _, err := splitHostPortLoose(v); err == nil {
			host = h
		}
		if !isLocalhostHost(host) {
			add("external_controller", "原始配置的 external-controller 监听非本地地址，Stage 7 渲染时将强制绑定 localhost")
		}
		if sec, ok := root["secret"].(string); !ok || sec == "" {
			add("controller_no_secret", "原始配置的 external-controller 缺少 secret，Stage 7 渲染时将生成并注入 controller secret")
		}
	}
	if tun, ok := root["tun"].(map[string]any); ok {
		if v, ok := tun["enable"].(bool); ok && v {
			add("tun_enabled", "原始配置启用了 TUN，Stage 7 渲染时将默认关闭")
		}
	}
	if dns, ok := root["dns"].(map[string]any); ok {
		if v, ok := dns["listen"].(string); ok && v != "" {
			host := v
			if h, _, err := splitHostPortLoose(v); err == nil {
				host = h
			}
			if !isLocalhostHost(host) {
				add("dns_external_listen", "原始配置的 DNS 监听非本地地址，Stage 7 渲染时将强制绑定 localhost")
			}
		}
	}
	return issues
}

// isLocalhostHost reports whether a bind/host value is loopback.
func isLocalhostHost(h string) bool {
	h = strings.TrimSpace(strings.Trim(h, "[]"))
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// splitHostPortLoose splits "host:port" tolerantly (missing port
// yields an error and the caller uses the raw value).
func splitHostPortLoose(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("no port")
	}
	return s[:i], s[i+1:], nil
}
