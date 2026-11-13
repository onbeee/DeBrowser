package service

import (
	"strings"
	"testing"
)

// Unit-level redaction cases complementing the ZIP-level test:
// raw (non-escaped) path forms, forward-slash Windows paths,
// space-containing segments, and content that must NOT be touched
// (URLs, timestamps, prose slashes, version strings).
func TestRedactPathsCases(t *testing.T) {
	cases := []struct{ in, mustContain, mustNotContain string }{
		{`open C:\Users\Bob\file.txt: denied`, "[REDACTED_PATH]: denied", "Bob"},
		{`path C:/Users/Bob/file.txt end`, "[REDACTED_PATH] end", "Bob"},
		{`visit https://example.com/a/b now`, "https://example.com/a/b", "REDACTED"},
		{`at 2026-11-13T13:12:30.146Z done`, "2026-11-13T13:12:30.146Z", "REDACTED"},
		{`this and/or that`, "and/or", "REDACTED"},
		{`{"k":"v1.19.34"}`, "v1.19.34", "REDACTED"},
		{`"dir":"/var/log/debrowser/run.log"`, "[REDACTED_PATH]", "/var/log"},
		{`error at /usr/local/bin/tool (code 3)`, "[REDACTED_PATH] (code 3)", "/usr/local"},
		{`D:\Games\Steam Library\app\run.exe failed`, "[REDACTED_PATH] failed", "Steam"},
	}
	for _, c := range cases {
		got := string(redactPaths([]byte(c.in)))
		if !strings.Contains(got, c.mustContain) {
			t.Errorf("redact(%q) = %q, want contains %q", c.in, got, c.mustContain)
		}
		if c.mustNotContain != "" && strings.Contains(got, c.mustNotContain) {
			t.Errorf("redact(%q) = %q, must not contain %q", c.in, got, c.mustNotContain)
		}
	}
}
