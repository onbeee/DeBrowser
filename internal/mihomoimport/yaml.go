package mihomoimport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }

// knownTopLevelKeys recognises a full Mihomo/Clash document: a
// mapping carrying at least one of these keys is a config; one
// carrying none of them is not something we can identify.
var knownTopLevelKeys = map[string]bool{
	"proxies": true, "proxy-groups": true, "rules": true, "dns": true,
	"port": true, "socks-port": true, "mixed-port": true, "redir-port": true,
	"tproxy-port": true, "external-controller": true, "secret": true,
	"tun": true, "sniffer": true, "profile": true, "hosts": true,
	"proxy-providers": true, "rule-providers": true, "listeners": true,
	"authentication": true, "log-level": true, "mode": true, "ipv6": true,
	"allow-lan": true, "bind-address": true, "external-ui": true,
}

// errNotYAML signals the detection layer to try the URI parser.
var errNotYAML = fmt.Errorf("not a YAML document")

// parseYAML parses a YAML payload (full config or proxies-only).
func parseYAML(data []byte) (*Parsed, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, errNotYAML
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errf("不支持包含多个文档的 YAML")
		}
		return nil, errf("YAML 格式错误：%v", yamlErrClass(err))
	}
	if len(doc.Content) == 0 {
		return nil, errNotYAML
	}
	if err := checkYAMLTree(&doc); err != nil {
		return nil, err
	}
	root := doc.Content[0]
	switch root.Kind {
	case yaml.MappingNode:
		return parseYAMLMapping(&doc, root)
	case yaml.SequenceNode:
		return parseYAMLSequence(&doc, root)
	default:
		return nil, errNotYAML
	}
}

// yamlErrClass reduces a yaml error to a safe class label (the raw
// error can quote document content, which may include secrets).
func yamlErrClass(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "mapping key") && strings.Contains(msg, "already defined"):
		return "存在重复的映射键"
	case strings.Contains(msg, "did not find expected"):
		return "缩进或结构不合法"
	case strings.Contains(msg, "cannot unmarshal"):
		return "字段类型不合法"
	default:
		return "语法不合法"
	}
}

// mappingEntry finds a key in a mapping node.
func mappingEntry(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func parseYAMLMapping(doc, root *yaml.Node) (*Parsed, error) {
	rootMap, err := nodeToAny(root)
	if err != nil {
		return nil, err
	}
	m, _ := rootMap.(map[string]any)
	if m == nil {
		return nil, errNotYAML
	}
	recognised := false
	for k := range m {
		if knownTopLevelKeys[k] {
			recognised = true
			break
		}
	}
	if !recognised {
		return nil, errf("无法识别该 YAML：不是受支持的 Mihomo 配置或代理节点列表")
	}
	format := FormatFullYAML
	onlyProxies := true
	for k := range m {
		if k != "proxies" && k != "proxy-groups" {
			onlyProxies = false
			break
		}
	}
	if onlyProxies {
		format = FormatProxiesYAML
	}

	out := &Parsed{Format: format, Bundle: map[string]string{}}
	out.Issues = securityReport(m)

	if seqNode := mappingEntry(root, "proxies"); seqNode != nil {
		nodes, err := extractYAMLCredentials(doc, root, seqNode, out)
		if err != nil {
			return nil, err
		}
		out.Nodes = nodes
	}
	if seqNode := mappingEntry(root, "proxy-groups"); seqNode != nil {
		groups, err := extractYAMLGroups(root, seqNode, out)
		if err != nil {
			return nil, err
		}
		out.Groups = groups
	}
	if err := validateNamesAndMembers(out.Nodes, out.Groups); err != nil {
		return nil, err
	}
	// Config-level extraction for everything outside the two
	// sections handled above (top-level secret, DNS/provider
	// credentials, URL userinfo anywhere else in the document).
	extractConfigSecrets(doc, out.Bundle)

	sanitized, err := marshalYAML(doc)
	if err != nil {
		return nil, errf("生成脱敏配置失败")
	}
	out.SanitizedYAML = sanitized
	return out, nil
}

func parseYAMLSequence(doc, root *yaml.Node) (*Parsed, error) {
	out := &Parsed{Format: FormatProxiesYAML, Bundle: map[string]string{}}
	nodes, err := extractYAMLNodes(root)
	if err != nil {
		return nil, err
	}
	out.Nodes = nodes
	if err := validateNamesAndMembers(out.Nodes, nil); err != nil {
		return nil, err
	}
	// Re-wrap as a mapping document {proxies: [...]} for storage.
	wrapped := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{
		Kind: yaml.MappingNode, Tag: "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "proxies"},
			root,
		},
	}}}
	sanitized, err := marshalYAML(wrapped)
	if err != nil {
		return nil, errf("生成脱敏配置失败")
	}
	out.SanitizedYAML = sanitized
	return out, nil
}

// extractYAMLCredentials pulls the proxies section into Node
// values and placeholder-replaces their secrets in the tree.
func extractYAMLCredentials(doc, root, seqNode *yaml.Node, out *Parsed) ([]Node, error) {
	return extractYAMLNodes(seqNode)
}

func extractYAMLNodes(seqNode *yaml.Node) ([]Node, error) {
	if seqNode.Kind != yaml.SequenceNode {
		return nil, errf("proxies 必须是一个列表")
	}
	var nodes []Node
	for i, item := range seqNode.Content {
		if item.Kind != yaml.MappingNode {
			return nil, errf("第 %d 个代理节点格式无效：必须是字段映射", i+1)
		}
		raw, err := nodeToAny(item)
		if err != nil {
			return nil, err
		}
		fields, _ := raw.(map[string]any)
		node, err := nodeFromFields(fields, i)
		if err != nil {
			return nil, err
		}
		// Placeholder the extracted secrets inside the persisted
		// document tree.
		placeholderSecrets(item)
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// extractYAMLGroups pulls the proxy-groups section into Group
// values, extracts option credentials into the config bundle and
// placeholders them in the tree.
func extractYAMLGroups(root, seqNode *yaml.Node, out *Parsed) ([]Group, error) {
	if seqNode.Kind != yaml.SequenceNode {
		return nil, errf("proxy-groups 必须是一个列表")
	}
	var groups []Group
	for i, item := range seqNode.Content {
		if item.Kind != yaml.MappingNode {
			return nil, errf("第 %d 个代理组格式无效：必须是字段映射", i+1)
		}
		raw, err := nodeToAny(item)
		if err != nil {
			return nil, err
		}
		fields, _ := raw.(map[string]any)
		name := stringField(fields, "name")
		typ := stringField(fields, "type")
		if name == "" {
			return nil, errf("第 %d 个代理组缺少 name", i+1)
		}
		if typ == "" {
			return nil, errf("代理组 %q 缺少 type", name)
		}
		var members []string
		if mv, present := fields["proxies"]; present {
			list, ok := mv.([]any)
			if !ok {
				return nil, errf("代理组 %q 的 proxies 必须是列表", name)
			}
			for _, el := range list {
				s, ok := el.(string)
				if !ok || s == "" {
					return nil, errf("代理组 %q 存在无效的成员条目", name)
				}
				members = append(members, s)
			}
		}
		options := map[string]any{}
		for k, v := range fields {
			if k == "name" || k == "type" || k == "proxies" {
				continue
			}
			options[k] = v
		}
		cfgOpts, secrets := classifyFields(options)
		for path, val := range secrets {
			out.Bundle[fmt.Sprintf("proxy-groups.%d.%s", i, path)] = val
		}
		placeholderSecrets(item)
		groups = append(groups, Group{Name: name, Type: typ, Options: cfgOpts, Members: members})
	}
	return groups, nil
}

// placeholderSecrets replaces, in place, every credential-named
// value inside a YAML subtree by the placeholder scalar.
func placeholderSecrets(n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			val := n.Content[i+1]
			if isSecretKey(key) {
				replaceScalar(val)
				continue
			}
			placeholderSecrets(val)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			placeholderSecrets(c)
		}
	case yaml.ScalarNode:
		if cleaned, ui := scrubUserinfo(n.Value); ui != "" {
			n.Value = cleaned
		}
	}
}

// nodeFromFields validates one node field set and classifies its
// credentials. idx is the zero-based position for error messages.
func nodeFromFields(fields map[string]any, idx int) (Node, error) {
	name := stringField(fields, "name")
	if name == "" {
		return Node{}, errf("第 %d 个代理节点缺少 name", idx+1)
	}
	typ := stringField(fields, "type")
	if typ == "" {
		return Node{}, errf("代理节点 %q 缺少 type", name)
	}
	protocol, ok := protocolByMihomoType[strings.ToLower(typ)]
	if !ok {
		return Node{}, errf("代理节点 %q 使用了不支持的协议类型 %q", name, typ)
	}
	server := stringField(fields, "server")
	if server == "" {
		return Node{}, errf("代理节点 %q 缺少 server", name)
	}
	port, ok := intField(fields, "port")
	if !ok || port < 1 || port > 65535 {
		return Node{}, errf("代理节点 %q 的 port 无效（必须在 1 到 65535 之间）", name)
	}
	rest := map[string]any{}
	for k, v := range fields {
		switch k {
		case "name", "type", "server", "port":
			continue
		}
		rest[k] = v
	}
	config, secrets := classifyFields(rest)
	return Node{
		Name: name, Type: strings.ToLower(typ), Protocol: protocol,
		Server: server, Port: port, Config: config, Secrets: secrets,
	}, nil
}

// validateNamesAndMembers enforces within-config name uniqueness
// and resolves group members by name: every member must be a node
// of THIS config, a group of THIS config, or a Mihomo builtin;
// group reference cycles are rejected.
func validateNamesAndMembers(nodes []Node, groups []Group) error {
	nodeNames := map[string]bool{}
	for _, n := range nodes {
		if nodeNames[n.Name] {
			return errf("代理节点名称重复：%q", n.Name)
		}
		nodeNames[n.Name] = true
	}
	groupNames := map[string]bool{}
	for _, g := range groups {
		if groupNames[g.Name] {
			return errf("代理组名称重复：%q", g.Name)
		}
		if nodeNames[g.Name] {
			return errf("代理组与代理节点重名，无法区分成员引用：%q", g.Name)
		}
		groupNames[g.Name] = true
	}
	edges := map[string][]string{}
	for _, g := range groups {
		for _, m := range g.Members {
			switch {
			case nodeNames[m], builtinMembers[m]:
			case groupNames[m]:
				edges[g.Name] = append(edges[g.Name], m)
			default:
				return errf("代理组 %q 引用了不存在的成员 %q", g.Name, m)
			}
		}
	}
	// Cycle detection over group -> group edges.
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(name string) error
	visit = func(name string) error {
		color[name] = grey
		for _, next := range edges[name] {
			switch color[next] {
			case grey:
				return errf("代理组之间存在循环引用（涉及 %q）", next)
			case white:
				if err := visit(next); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}
	for _, g := range groups {
		if color[g.Name] == white {
			if err := visit(g.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// marshalYAML renders a document node tree back to YAML bytes.
func marshalYAML(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// intField coerces YAML int / float / string forms of a number.
func intField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		if t == float64(int(t)) {
			return int(t), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}
