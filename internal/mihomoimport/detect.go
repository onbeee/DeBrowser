package mihomoimport

import (
	"bytes"
	"encoding/base64"
	"strings"
)

// Parse is the single entry point of the import pipeline. It
// recognises the payload format by content (never by file name):
// full Mihomo YAML, proxies-only YAML, a URI list, or a Base64
// wrapping of any of those. Unsupported or malformed content is
// rejected with a specific error — nothing is silently dropped.
func Parse(data []byte) (*Parsed, error) {
	if len(data) > MaxInputBytes {
		return nil, errf("输入超过 10 MiB 上限")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errf("输入为空")
	}
	// A Base64-wrapped payload is a single whitespace-free token.
	if !bytes.ContainsAny(trimmed, " \t\r\n") {
		if decoded, ok := decodeBase64Token(trimmed); ok {
			if p, err := parsePlain(decoded); err == nil {
				return p, nil
			}
			// Not actually a wrapped payload — fall through and
			// let the plain parsers report the real problem.
		}
	}
	return parsePlain(data)
}

// parsePlain parses a non-wrapped payload: YAML first, URI list
// second.
func parsePlain(data []byte) (*Parsed, error) {
	p, err := parseYAML(data)
	if err == nil {
		return p, nil
	}
	if err != errNotYAML {
		return nil, err
	}
	return parseURIList(data)
}

// decodeBase64Token tries the standard and URL-safe alphabets,
// padded and unpadded, and reports the decoded bytes.
func decodeBase64Token(token []byte) ([]byte, bool) {
	if len(token) < 16 {
		return nil, false
	}
	for _, r := range string(token) {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '+' || r == '/' || r == '-' || r == '_' || r == '=') {
			return nil, false
		}
	}
	encs := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, enc := range encs {
		if decoded, err := enc.DecodeString(string(token)); err == nil && len(decoded) > 0 {
			// The decoded content must look like a payload (URI
			// list or YAML), not arbitrary binary.
			d := bytes.TrimSpace(decoded)
			if bytes.Contains(d, []byte("://")) || strings.HasPrefix(string(d), "proxies") ||
				bytes.Contains(d, []byte("\n")) {
				return decoded, true
			}
		}
	}
	return nil, false
}

// DecodeBase64Flexible decodes Base64 in any common variant; used
// by the URI parsers for vmess and Shadowsocks payloads.
func DecodeBase64Flexible(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	encs := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	var lastErr error
	for _, enc := range encs {
		if decoded, err := enc.DecodeString(s); err == nil {
			return decoded, nil
		} else {
			lastErr = err
		}
	}
	return nil, lastErr
}
