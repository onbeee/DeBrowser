// Package mihomoimport is the pure parsing pipeline for Stage 6
// subscription and config imports. It converts subscription
// payloads — full Clash/Mihomo YAML, proxies-only YAML, and
// V2Ray/Base64 URI lists — into a validated, credential-extracted
// form the service layer can persist.
//
// The package is pure: it never touches the database, the vault or
// the filesystem. Credentials found anywhere in a document are
// extracted into Node.Secrets (per node) or Parsed.Bundle (config
// level, addressed by document path), and the returned
// SanitizedYAML carries an explicit placeholder wherever a
// credential was removed, so no plaintext secret survives in the
// parse result.
package mihomoimport

import (
	"fmt"
	"strings"
)

// Format identifies which input shape a payload was recognised as.
type Format int

const (
	FormatFullYAML Format = iota + 1
	FormatProxiesYAML
	FormatURIList
)

// String returns the stable machine name used in import summaries.
func (f Format) String() string {
	switch f {
	case FormatFullYAML:
		return "mihomo_yaml"
	case FormatProxiesYAML:
		return "proxies_yaml"
	case FormatURIList:
		return "uri_list"
	}
	return "unknown"
}

// MaxInputBytes bounds a single import payload (10 MiB).
const MaxInputBytes = 10 << 20

// Placeholder replaces extracted credential values inside the
// sanitized document. It is deliberately not a valid credential
// for anything; Stage 7 rendering substitutes real values from
// the vault under its own rules.
const Placeholder = "__VAULT__"

// Node is one proxy node extracted from a document.
type Node struct {
	Name     string            // name inside the source document
	Type     string            // original Mihomo type (ss, vmess, ...)
	Protocol string            // mapped database protocol
	Server   string            //
	Port     int               //
	Config   map[string]any    // non-secret fields only (JSON-safe)
	Secrets  map[string]string // credential fields, dotted paths for nested ones
}

// Group is one native Mihomo proxy-group with member names in
// document order. The service layer resolves names to stable IDs.
type Group struct {
	Name    string
	Type    string
	Options map[string]any // non-secret options only
	Members []string
}

// SecurityIssue is one Stage 6 finding about a full config. The
// report records the risk only; Stage 7 enforces the safe values
// when it renders a runnable config.
type SecurityIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Parsed is the complete result of parsing one payload.
type Parsed struct {
	Format        Format
	Nodes         []Node
	Groups        []Group
	SanitizedYAML []byte
	// Bundle holds config-level credentials extracted from the
	// document, keyed by document path (for example
	// "dns.fallback.0" or "proxy-groups.2.password"). Node
	// credentials are NOT here — they are in Node.Secrets.
	Bundle map[string]string
	Issues []SecurityIssue
}

// protocolByMihomoType maps Mihomo outbound types onto the
// database protocol vocabulary (migration 0002/0005 CHECK list).
var protocolByMihomoType = map[string]string{
	"ss":          "shadowsocks",
	"shadowsocks": "shadowsocks",
	"http":        "http",
	"socks5":      "socks5",
	"vmess":       "vmess",
	"vless":       "vless",
	"trojan":      "trojan",
	"hysteria2":   "hysteria2",
	"tuic":        "tuic",
	"wireguard":   "wireguard",
}

// builtinMembers are the Mihomo built-in group members that need
// no resolution to a node or group.
var builtinMembers = map[string]bool{
	"DIRECT": true, "REJECT": true, "REJECT-DROP": true,
	"PASS": true, "COMPATIBLE": true, "GLOBAL": true,
}

// IsBuiltinMember reports whether name is a Mihomo builtin member.
func IsBuiltinMember(name string) bool { return builtinMembers[name] }

// secretKeyExact names credential fields (compared lower-cased).
// The set aligns with the ProxyService config_json guard
// (secretConfigKeys) extended by the Stage 6 instruction list:
// identity fields (username/user), auth material and passphrase
// variants are credentials too and must never sit in plaintext
// config JSON.
var secretKeyExact = map[string]bool{
	"password": true, "passwd": true, "pwd": true, "passphrase": true,
	"uuid":        true,
	"private-key": true, "private_key": true, "privatekey": true,
	"preshared-key": true, "preshared_key": true, "psk": true,
	"obfs-password": true, "obfs_password": true,
	"token": true, "secret": true,
	"auth": true, "auth-str": true, "auth_str": true,
	"username": true, "user": true,
	"access-token": true, "access_token": true,
	"api-key": true, "api_key": true,
	"client-secret": true, "client_secret": true,
}

// isSecretKey reports whether a field name denotes a credential.
// Beyond the exact set, any name containing "password" or "secret"
// or ending in a token/psk suffix is treated conservatively as a
// credential (Stage 6 instruction: when in doubt, extract).
func isSecretKey(key string) bool {
	lk := strings.ToLower(key)
	if secretKeyExact[lk] {
		return true
	}
	if strings.Contains(lk, "password") || strings.Contains(lk, "secret") {
		return true
	}
	if strings.HasSuffix(lk, "-token") || strings.HasSuffix(lk, "_token") ||
		strings.HasSuffix(lk, "-psk") || strings.HasSuffix(lk, "_psk") ||
		strings.HasSuffix(lk, "-key") && strings.Contains(lk, "private") {
		return true
	}
	return false
}

// errf formats an import error. Messages are user-facing Chinese
// and must never embed payload content, URIs or credential values.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
