package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/paths"
)

func testSystem(t *testing.T) (*System, paths.Layout) {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	sys, err := NewSystem(config.Config{Layout: layout, LogLevel: "debug"})
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	return sys, layout
}

func readCategory(t *testing.T, layout paths.Layout, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(layout.Logs)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix+"-") {
			data, err := os.ReadFile(filepath.Join(layout.Logs, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			sb.Write(data)
		}
	}
	return sb.String()
}

func TestSystemThreeCategories(t *testing.T) {
	sys, layout := testSystem(t)
	sys.Logger().Info("app started")
	sys.Logger().Error("something failed", "error", errors.New("boom"))
	sys.Audit(AuditEvent{Operation: "instance.create", TargetType: "instance", TargetID: "abc", OK: true, Duration: 1500 * time.Microsecond})
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}

	run := readCategory(t, layout, "run")
	if !strings.Contains(run, `"category":"run"`) || !strings.Contains(run, `"event":"app started"`) {
		t.Fatalf("run log missing the info record:\n%s", run)
	}
	if !strings.Contains(run, `"timestamp":`) || !strings.Contains(run, `"level":"INFO"`) {
		t.Fatalf("run record lacks the Stage 5B schema fields:\n%s", run)
	}
	errLog := readCategory(t, layout, "error")
	if !strings.Contains(errLog, `"category":"error"`) || !strings.Contains(errLog, "something failed") {
		t.Fatalf("error log missing the tee'd error record:\n%s", errLog)
	}
	op := readCategory(t, layout, "operation")
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(op), "\n") {
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("operation line is not JSON: %v (%q)", err, line)
		}
	}
	if rec["operation"] != "instance.create" || rec["result"] != "success" || rec["targetId"] != "abc" {
		t.Fatalf("audit record = %v", rec)
	}
	if _, hasErr := rec["error"]; hasErr {
		t.Fatalf("successful audit record carries an error field: %v", rec)
	}
}

func TestAuditFailureRecordWhitelist(t *testing.T) {
	sys, layout := testSystem(t)
	sys.Audit(AuditEvent{Operation: "proxy.delete", TargetType: "proxy", TargetID: "p1", OK: false, ErrClass: "not_found"})
	_ = sys.Close()
	op := readCategory(t, layout, "operation")
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(op)), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["result"] != "failure" || rec["error"] != "not_found" {
		t.Fatalf("audit failure record = %v", rec)
	}
	// The record may ONLY contain whitelist fields.
	allowed := map[string]bool{"timestamp": true, "level": true, "category": true, "event": true,
		"operation": true, "targetType": true, "targetId": true, "result": true, "durationMs": true, "error": true}
	for k := range rec {
		if !allowed[k] {
			t.Fatalf("audit record contains non-whitelist field %q", k)
		}
	}
}

func TestGuardDropsSecrets(t *testing.T) {
	sys, layout := testSystem(t)
	secretURL := "https://user:pass123@example.com/sub?token=abcdef"
	sys.Logger().Info("connecting",
		"password", "hunter2",
		"secret_ref", "vault-ref-123",
		"url", secretURL,
		"error", fmt.Errorf("dial failed for %s password=zzz", secretURL),
	)
	sys.Logger().Error("vault open failed", "error",
		fmt.Errorf("decrypt blob %s failed", strings.Repeat("A", 80)))
	_ = sys.Close()
	all := readCategory(t, layout, "run") + readCategory(t, layout, "error")
	for _, forbidden := range []string{"hunter2", "vault-ref-123", "pass123", "token=abcdef", "password=zzz", strings.Repeat("A", 80)} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("log contains forbidden content %q:\n%s", forbidden, all)
		}
	}
	if !strings.Contains(all, "connecting") {
		t.Fatalf("legitimate record missing:\n%s", all)
	}
}

func TestScrubErrorText(t *testing.T) {
	if got := ScrubErrorText("dial https://user:pw@host.example/x now"); strings.Contains(got, "pw@") || !strings.Contains(got, "***@host.example") {
		t.Fatalf("userinfo not scrubbed: %q", got)
	}
	if got := ScrubErrorText("auth failed password=abc123 end"); strings.Contains(got, "abc123") {
		t.Fatalf("kv secret not scrubbed: %q", got)
	}
	long := ScrubErrorText(strings.Repeat("word ", 200))
	if len(long) > 520 {
		t.Fatalf("scrubbed error not truncated: %d chars", len(long))
	}
}

func TestSystemStatsAndPolicies(t *testing.T) {
	sys, _ := testSystem(t)
	sys.Logger().Info("x")
	stats := sys.Stats()
	if len(stats) != 3 {
		t.Fatalf("stats = %d categories, want 3", len(stats))
	}
	for _, st := range stats {
		switch st.Category {
		case CategoryRun:
			if st.Policy.RetentionDays != 30 || st.Policy.CapBytes != 20<<20 {
				t.Fatalf("run policy = %+v", st.Policy)
			}
			if st.Files < 1 {
				t.Fatalf("run stats files = %d", st.Files)
			}
		case CategoryOperation, CategoryError:
			if st.Policy.RetentionDays != 90 || st.Policy.CapBytes != 50<<20 {
				t.Fatalf("%s policy = %+v", st.Category, st.Policy)
			}
		}
	}
}
