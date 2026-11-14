package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/logging"
)

// Stage 5B: user-initiated instance mutations produce whitelist
// audit records in the operation log — and never leak credentials
// or secret references into any log.
func TestOperationAuditRecords(t *testing.T) {
	instSvc, cfg := testInstanceService(t)
	logs, err := logging.NewSystem(cfg)
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	instSvc.Auditor = logs

	inst, err := instSvc.CreateInstance(CreateInstanceInput{Name: "审计实例"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if _, err := instSvc.UpdateInstance(UpdateInstanceInput{ID: inst.ID, Name: "审计实例改名"}); err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if err := instSvc.DeleteInstance(inst.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if _, err := instSvc.CreateInstance(CreateInstanceInput{Name: "  "}); err == nil {
		t.Fatal("empty-name create unexpectedly succeeded")
	}

	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}

	opLog := readLogsOf(t, cfg.Layout.Logs, "operation")
	for _, want := range []string{
		`"operation":"instance.create"`, `"operation":"instance.update"`, `"operation":"instance.delete"`,
		`"result":"success"`, `"result":"failure"`,
	} {
		if !strings.Contains(opLog, want) {
			t.Fatalf("operation log missing %q:\n%s", want, opLog)
		}
	}
	// No log of any category may contain a secret reference or the
	// free-form instance name the user typed.
	all := opLog + readLogsOf(t, cfg.Layout.Logs, "run") + readLogsOf(t, cfg.Layout.Logs, "error")
	for _, forbidden := range []string{"secret_ref", "审计实例"} {
		if strings.Contains(all, forbidden) {
			t.Fatalf("logs contain forbidden content %q", forbidden)
		}
	}
}

func readLogsOf(t *testing.T, logsDir, prefix string) string {
	t.Helper()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix+"-") {
			data, err := os.ReadFile(filepath.Join(logsDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			sb.Write(data)
		}
	}
	return sb.String()
}
