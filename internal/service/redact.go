package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
)

// Path redaction for diagnostic exports (Stage 5B review fix).
//
// Log files written before the startup record stopped carrying
// absolute paths — and error messages generated anywhere in the
// program — can still contain the user's filesystem layout. Since
// diagnostic ZIPs leave the machine, every log file passes through
// redactPaths IN MEMORY at export time; the bytes on disk are
// never modified. The redacted bytes are what get validated,
// scanned, hashed and archived, so the manifest describes exactly
// what ships.
//
// The patterns are structural rather than tied to specific field
// names or directories:
//
//   - Windows drive paths, with single or JSON-escaped (doubled)
//     separators and either slash direction: C:\Users\…,
//     D:/Private/…, and C:\\Users\\… as it appears in raw JSON text.
//   - UNC paths: \\server\share\… (and its JSON-escaped form).
//   - Unix-style absolute paths: /home/…, /Users/…, /tmp/…, …
//
// A segment may contain spaces (e.g. "Application Support") only
// when another segment follows it, so trailing prose after a path
// ("… plus other text") is not swallowed. URLs are safe: a drive
// or root match must start at a value boundary, so a scheme like
// "https://" is never mistaken for a path, and dates/times contain
// neither a leading slash nor a drive prefix.
const redactedPath = "[REDACTED_PATH]"

var (
	// pathAtom is one path segment without spaces. The charset
	// excludes every character that can terminate a path inside
	// JSON text or an error message, so matches end cleanly.
	pathAtom = `[^\s"\\/:*?<>|]+`
	// spaceSeg is a segment containing spaces ("Application
	// Support"); it is only used in a non-final position.
	spaceSeg = pathAtom + `(?: ` + pathAtom + `)+`
	// seg is any non-final segment.
	seg = `(?:` + pathAtom + `|` + spaceSeg + `)`
	// boundary is what may precede a path: start of data, JSON
	// punctuation, whitespace or an opening bracket — never a word
	// character, so matches cannot start inside a word or a URL.
	boundary = `(^|[\s"'(=\x60])`

	uncPathRe = regexp.MustCompile(`\\{2,4}(?:` + seg + `(?:\\{1,2}|/))*` + pathAtom)
	winPathRe = regexp.MustCompile(boundary + `[A-Za-z]:(?:\\{1,2}|/)(?:` + seg + `(?:\\{1,2}|/))*` + pathAtom)
	// A bare drive root like "C:\" with nothing after it.
	winRootRe  = regexp.MustCompile(boundary + `[A-Za-z]:(?:\\{1,2}|/)`)
	unixPathRe = regexp.MustCompile(boundary + `/(?:` + seg + `/)*` + pathAtom)
)

// redactPaths replaces every absolute-path-shaped substring of
// data with a stable placeholder. Non-path content — URLs,
// timestamps, ordinary prose — is preserved.
func redactPaths(data []byte) []byte {
	out := uncPathRe.ReplaceAll(data, []byte(redactedPath))
	out = winPathRe.ReplaceAll(out, []byte("$1"+redactedPath))
	out = winRootRe.ReplaceAll(out, []byte("$1"+redactedPath))
	out = unixPathRe.ReplaceAll(out, []byte("$1"+redactedPath))
	return out
}

// validateJSONLines verifies that data is well-formed JSON Lines:
// every non-empty line must be valid JSON. Exported logs keep
// their format guarantee — redaction must never corrupt a record.
func validateJSONLines(data []byte) error {
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			return fmt.Errorf("第 %d 行不是合法的 JSON 记录", i+1)
		}
	}
	return nil
}
