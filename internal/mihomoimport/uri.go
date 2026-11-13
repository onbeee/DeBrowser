package mihomoimport

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseURIList parses one-URI-per-line subscription text. Blank
// lines and #-comments are skipped; every other line must be a
// fully supported URI — a single unsupported or malformed entry
// fails the whole import (Stage 6 instruction: never drop entries
// silently). Error messages carry line numbers and schemes only,
// never URI content.
func parseURIList(data []byte) (*Parsed, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	var nodes []Node
	nonEmpty := 0
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		nonEmpty++
		node, err := parseURI(line)
		if err != nil {
			return nil, errf("第 %d 行：%v", i+1, err)
		}
		nodes = append(nodes, node)
	}
	if nonEmpty == 0 {
		return nil, errf("输入中没有可识别的订阅内容")
	}
	if len(nodes) == 0 {
		return nil, errf("输入中没有有效的代理节点")
	}
	// Any credential-shaped key a parser left in Config (for
	// example a hysteria2 obfs-password from the query string) is
	// extracted here, once, for every URI node.
	for i := range nodes {
		cfg, extra := classifyFields(nodes[i].Config)
		nodes[i].Config = cfg
		for k, v := range extra {
			nodes[i].Secrets[k] = v
		}
	}
	if err := validateNamesAndMembers(nodes, nil); err != nil {
		return nil, err
	}
	out := &Parsed{Format: FormatURIList, Nodes: nodes, Bundle: map[string]string{}}
	sanitized, err := synthesizeURIDocument(nodes)
	if err != nil {
		return nil, errf("生成脱敏配置失败")
	}
	out.SanitizedYAML = sanitized
	return out, nil
}

// parseURI parses a single subscription URI by scheme.
func parseURI(line string) (Node, error) {
	idx := strings.Index(line, "://")
	if idx < 0 {
		return Node{}, errf("不是有效的 URI")
	}
	scheme := strings.ToLower(line[:idx])
	switch scheme {
	case "vmess":
		return parseVMessURI(line)
	case "ss":
		return parseSSURI(line)
	case "vless", "trojan", "tuic":
		return parseUserinfoURI(line, scheme)
	case "hysteria2", "hy2":
		return parseUserinfoURI(line, "hysteria2")
	case "socks5", "http", "https":
		return parseUserinfoURI(line, scheme)
	default:
		return Node{}, errf("不支持的协议 %q", scheme)
	}
}

// base returns a Node skeleton with defaults filled after server
// and port validation.
func uriNode(name, typ, protocol, server string, port int) Node {
	if name == "" {
		name = fmt.Sprintf("%s-%s-%d", protocol, server, port)
	}
	return Node{
		Name: name, Type: typ, Protocol: protocol,
		Server: server, Port: port,
		Config: map[string]any{}, Secrets: map[string]string{},
	}
}

// hostPortOf extracts and validates host/port from a parsed URL,
// covering IPv4, hostnames and bracketed IPv6.
func hostPortOf(u *url.URL) (string, int, error) {
	host := u.Hostname()
	if host == "" {
		return "", 0, errf("缺少服务器地址")
	}
	portStr := u.Port()
	if portStr == "" {
		return "", 0, errf("缺少端口")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errf("端口无效")
	}
	return host, port, nil
}

// parseVMessURI handles vmess://<base64 JSON>.
func parseVMessURI(line string) (Node, error) {
	payload := strings.TrimPrefix(line, "vmess://")
	if i := strings.Index(payload, "#"); i >= 0 {
		payload = payload[:i]
	}
	raw, err := DecodeBase64Flexible(payload)
	if err != nil {
		return Node{}, errf("vmess 节点的 Base64 无效")
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return Node{}, errf("vmess 节点的 JSON 无效")
	}
	server := stringField(v, "add")
	if server == "" {
		return Node{}, errf("vmess 节点缺少服务器地址（add）")
	}
	port, ok := intField(v, "port")
	if !ok || port < 1 || port > 65535 {
		return Node{}, errf("vmess 节点端口无效")
	}
	uuid := stringField(v, "id")
	if uuid == "" {
		return Node{}, errf("vmess 节点缺少 UUID（id）")
	}
	node := uriNode(stringField(v, "ps"), "vmess", "vmess", server, port)
	node.Secrets["uuid"] = uuid
	if aid, ok := intField(v, "aid"); ok {
		node.Config["alterId"] = aid
	}
	node.Config["cipher"] = "auto"
	network := stringField(v, "net")
	if network == "" {
		network = "tcp"
	}
	node.Config["network"] = network
	if stringField(v, "tls") == "tls" {
		node.Config["tls"] = true
		if sni := stringField(v, "sni"); sni != "" {
			node.Config["servername"] = sni
		}
	}
	host, path := stringField(v, "host"), stringField(v, "path")
	switch network {
	case "ws":
		opts := map[string]any{}
		if path != "" {
			opts["path"] = path
		}
		if host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		if len(opts) > 0 {
			node.Config["ws-opts"] = opts
		}
	case "grpc":
		if path != "" {
			node.Config["grpc-opts"] = map[string]any{"grpc-service-name": path}
		}
	case "h2":
		opts := map[string]any{}
		if path != "" {
			opts["path"] = path
		}
		if host != "" {
			opts["host"] = []any{host}
		}
		if len(opts) > 0 {
			node.Config["h2-opts"] = opts
		}
	}
	return node, nil
}

// parseSSURI handles Shadowsocks URIs in SIP002 and legacy forms.
func parseSSURI(line string) (Node, error) {
	rest := strings.TrimPrefix(line, "ss://")
	name := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		if decoded, err := url.PathUnescape(rest[i+1:]); err == nil {
			name = decoded
		}
		rest = rest[:i]
	}
	if !strings.Contains(rest, "@") {
		// Legacy form: the whole authority is Base64 and decodes
		// to method:password@host:port.
		decoded, err := DecodeBase64Flexible(rest)
		if err != nil {
			return Node{}, errf("ss 节点的 Base64 无效")
		}
		plain := string(decoded)
		at := strings.LastIndex(plain, "@")
		if at < 0 {
			return Node{}, errf("ss 节点格式无效")
		}
		method, password := splitMethodPassword(plain[:at])
		return finishSS(name, method, password, plain[at+1:], "")
	}
	u, err := url.Parse(line)
	if err != nil {
		return Node{}, errf("ss 节点 URI 无效")
	}
	if _, _, err := hostPortOf(u); err != nil {
		return Node{}, err
	}
	if u.User == nil {
		return Node{}, errf("ss 节点缺少认证信息")
	}
	var method, password string
	user := u.User.Username()
	if pw, has := u.User.Password(); has {
		// Plain userinfo: ss://method:password@host:port
		method, password = user, pw
	} else if decoded, err := DecodeBase64Flexible(user); err == nil {
		// SIP002: ss://base64(method:password)@host:port
		method, password = splitMethodPassword(string(decoded))
	} else {
		return Node{}, errf("ss 节点认证信息无效")
	}
	return finishSS(name, method, password, u.Host, u.Query().Get("plugin"))
}

func splitMethodPassword(creds string) (string, string) {
	i := strings.Index(creds, ":")
	if i < 0 {
		return creds, ""
	}
	return creds[:i], creds[i+1:]
}

// finishSS builds the ss node from its parts.
func finishSS(name, method, password, hostport, plugin string) (Node, error) {
	var host string
	var port int
	if strings.HasPrefix(hostport, "[") {
		u, err := url.Parse("ss://" + hostport)
		if err != nil {
			return Node{}, errf("ss 节点地址无效")
		}
		h, p, herr := hostPortOf(u)
		if herr != nil {
			return Node{}, herr
		}
		host, port = h, p
	} else {
		h, portStr, err := splitHostPortLoose(hostport)
		if err != nil {
			return Node{}, errf("ss 节点地址无效")
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 || p > 65535 {
			return Node{}, errf("ss 节点端口无效")
		}
		host, port = h, p
	}
	if method == "" {
		return Node{}, errf("ss 节点缺少加密方法")
	}
	if password == "" {
		return Node{}, errf("ss 节点缺少密码")
	}
	node := uriNode(name, "ss", "shadowsocks", host, port)
	node.Secrets["password"] = password
	node.Config["cipher"] = method
	if plugin != "" {
		parts := strings.Split(plugin, ";")
		node.Config["plugin"] = parts[0]
		opts := map[string]any{}
		for _, kv := range parts[1:] {
			if eq := strings.Index(kv, "="); eq >= 0 {
				opts[kv[:eq]] = kv[eq+1:]
			} else if kv != "" {
				opts[kv] = true
			}
		}
		if len(opts) > 0 {
			node.Config["plugin-opts"] = opts
		}
	}
	return node, nil
}

// parseUserinfoURI covers the URL-shaped schemes: vless, trojan,
// tuic, hysteria2, socks5, http, https.
func parseUserinfoURI(line, scheme string) (Node, error) {
	u, err := url.Parse(line)
	if err != nil {
		return Node{}, errf("%s 节点 URI 无效", scheme)
	}
	host, port, err := hostPortOf(u)
	if err != nil {
		return Node{}, err
	}
	protocol := protocolByMihomoType[scheme]
	typ := scheme
	if scheme == "https" {
		protocol = "https"
	}
	node := uriNode(u.Fragment, typ, protocol, host, port)
	q := u.Query()

	switch scheme {
	case "vless":
		if u.User == nil || u.User.Username() == "" {
			return Node{}, errf("vless 节点缺少 UUID")
		}
		node.Secrets["uuid"] = u.User.Username()
		applyTransportQuery(&node, q)
		if flow := q.Get("flow"); flow != "" {
			node.Config["flow"] = flow
		}
		security := q.Get("security")
		if security == "tls" || security == "reality" {
			node.Config["tls"] = true
		}
		if security == "reality" {
			opts := map[string]any{}
			if pbk := q.Get("pbk"); pbk != "" {
				opts["public-key"] = pbk
			}
			if sid := q.Get("sid"); sid != "" {
				opts["short-id"] = sid
			}
			node.Config["reality-opts"] = opts
		}
	case "trojan":
		if u.User == nil || u.User.Username() == "" {
			return Node{}, errf("trojan 节点缺少密码")
		}
		node.Secrets["password"] = u.User.Username()
		applyTransportQuery(&node, q)
	case "tuic":
		if u.User == nil || u.User.Username() == "" {
			return Node{}, errf("tuic 节点缺少 UUID")
		}
		node.Secrets["uuid"] = u.User.Username()
		if pw, has := u.User.Password(); has && pw != "" {
			node.Secrets["password"] = pw
		}
		if cc := q.Get("congestion_control"); cc != "" {
			node.Config["congestion-controller"] = cc
		}
		if alpn := q.Get("alpn"); alpn != "" {
			node.Config["alpn"] = splitCommaList(alpn)
		}
		if sni := q.Get("sni"); sni != "" {
			node.Config["sni"] = sni
		}
	case "hysteria2":
		if u.User != nil && u.User.Username() != "" {
			auth := u.User.Username()
			if pw, has := u.User.Password(); has {
				auth = auth + ":" + pw
			}
			node.Secrets["password"] = auth
		}
		if sni := q.Get("sni"); sni != "" {
			node.Config["sni"] = sni
		}
		if q.Get("insecure") == "1" {
			node.Config["skip-cert-verify"] = true
		}
		if obfs := q.Get("obfs"); obfs != "" {
			node.Config["obfs"] = obfs
		}
		if obfsPw := q.Get("obfs-password"); obfsPw != "" {
			node.Secrets["obfs-password"] = obfsPw
		}
		if ports := q.Get("ports"); ports != "" {
			node.Config["ports"] = ports
		}
	case "socks5", "http", "https":
		if u.User != nil && u.User.Username() != "" {
			node.Secrets["username"] = u.User.Username()
			if pw, has := u.User.Password(); has && pw != "" {
				node.Secrets["password"] = pw
			}
		}
		if scheme != "socks5" && q.Get("security") == "tls" {
			node.Config["tls"] = true
		}
	}
	return node, nil
}

// applyTransportQuery maps the shared transport query parameters
// (type/sni/host/path/serviceName/fp) onto Mihomo node fields.
func applyTransportQuery(node *Node, q url.Values) {
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	node.Config["network"] = network
	if sni := firstOf(q.Get("sni"), q.Get("servername")); sni != "" {
		node.Config["servername"] = sni
	}
	if fp := q.Get("fp"); fp != "" {
		node.Config["client-fingerprint"] = fp
	}
	host, path := q.Get("host"), q.Get("path")
	switch network {
	case "ws":
		opts := map[string]any{}
		if path != "" {
			opts["path"] = path
		}
		if host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		if len(opts) > 0 {
			node.Config["ws-opts"] = opts
		}
	case "grpc":
		if sn := q.Get("serviceName"); sn != "" {
			node.Config["grpc-opts"] = map[string]any{"grpc-service-name": sn}
		}
	case "h2":
		opts := map[string]any{}
		if path != "" {
			opts["path"] = path
		}
		if host != "" {
			opts["host"] = []any{host}
		}
		if len(opts) > 0 {
			node.Config["h2-opts"] = opts
		}
	}
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func splitCommaList(s string) []any {
	parts := strings.Split(s, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// synthesizeURIDocument renders the sanitized artifact for a URI
// import: a proxies-only YAML document whose secret fields carry
// the placeholder, mirroring what a YAML import would persist.
func synthesizeURIDocument(nodes []Node) ([]byte, error) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, n := range nodes {
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		addPair := func(k string, v *yaml.Node) {
			m.Content = append(m.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, v)
		}
		addPair("name", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: n.Name})
		addPair("type", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: n.Type})
		addPair("server", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: n.Server})
		addPair("port", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(n.Port)})
		for _, k := range sortedKeys(n.Config) {
			v, err := anyToYAMLNode(n.Config[k])
			if err != nil {
				return nil, err
			}
			addPair(k, v)
		}
		// Secrets are recorded as placeholders, re-nested along
		// their dotted paths.
		ph := map[string]any{}
		for path := range n.Secrets {
			setDotted(ph, path, Placeholder)
		}
		for _, k := range sortedKeys(ph) {
			v, err := anyToYAMLNode(ph[k])
			if err != nil {
				return nil, err
			}
			addPair(k, v)
		}
		seq.Content = append(seq.Content, m)
	}
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{
		Kind: yaml.MappingNode, Tag: "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "proxies"},
			seq,
		},
	}}}
	return marshalYAML(doc)
}

// setDotted writes value into root at a dotted path, creating
// intermediate mappings.
func setDotted(root map[string]any, path string, value any) {
	parts := strings.Split(path, ".")
	cur := root
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = value
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// anyToYAMLNode converts plain Go values back into a YAML node
// tree for artifact synthesis.
func anyToYAMLNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range sortedKeys(t) {
			child, err := anyToYAMLNode(t[k])
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, child)
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, el := range t {
			child, err := anyToYAMLNode(el)
			if err != nil {
				return nil, err
			}
			n.Content = append(n.Content, child)
		}
		return n, nil
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t}, nil
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(t)}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}, nil
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(t, 'g', -1, 64)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unsupported value type %T", v)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
