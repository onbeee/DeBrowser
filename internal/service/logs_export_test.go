package service

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/settings"
)

// seedOperationWithError records one full operation chain ending
// in a failure, through the real logging system of the fixture.
func seedOperationWithError(t *testing.T, f diagFixture, opID string) {
	t.Helper()
	op := f.logs.BeginOperation(opID, "浏览器实例", "instance.create", "instance", "", "user")
	op.Step("校验输入", logging.StatusSuccess, nil)
	op.Fail(errString("实例名称不能为空"), "operation_failed", "failed", "实例名称不能为空")
}

type errString string

func (e errString) Error() string { return string(e) }

func openZip(t *testing.T, path string) map[string][]byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer func() { _ = r.Close() }()
	out := map[string][]byte{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 0, f.UncompressedSize64)
		buf := make([]byte, 64<<10)
		for {
			n, err := rc.Read(buf)
			data = append(data, buf[:n]...)
			if err != nil {
				break
			}
		}
		_ = rc.Close()
		out[f.Name] = data
	}
	return out
}

func TestLogExportInfoDynamicRange(t *testing.T) {
	f := newDiagFixture(t)
	// No records yet in a fresh system apart from slog lines in
	// the run stream; the operation category must be empty.
	info, err := f.svc.GetLogExportInfo("operation")
	if err != nil {
		t.Fatal(err)
	}
	if info.HasRecords {
		t.Fatalf("fresh operation category reports records: %+v", info)
	}
	f.logs.RecordEvent(logging.Event{
		EventType: logging.EventTypeOperationStart, Category: logging.EventCategoryOperation,
		Module: "测试", Action: "a.start", Timestamp: "2026-10-20T08:00:00+08:00",
	})
	f.logs.RecordEvent(logging.Event{
		EventType: logging.EventTypeOperationResult, Category: logging.EventCategoryOperation,
		Module: "测试", Action: "a.result", Timestamp: "2026-11-15T09:00:00+08:00", Status: logging.StatusSuccess,
	})
	info, err = f.svc.GetLogExportInfo("operation")
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasRecords || info.RecordCount != 2 {
		t.Fatalf("info = %+v", info)
	}
	if info.EarliestDate != "2026-10-20" || info.LatestDate != "2026-11-15" {
		t.Fatalf("range = %s .. %s", info.EarliestDate, info.LatestDate)
	}
	if _, err := f.svc.GetLogExportInfo("bogus"); err == nil {
		t.Fatal("bogus category accepted")
	}
}

func TestExportRecordsErrorWithContext(t *testing.T) {
	f := newDiagFixture(t)
	seedOperationWithError(t, f, "op-export-1")
	dest := filepath.Join(t.TempDir(), "out.zip")
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	res, err := f.svc.ExportRecords("", "error", "", "")
	if err != nil {
		t.Fatalf("ExportRecords: %v", err)
	}
	if res.Cancelled || res.Path != dest {
		t.Fatalf("result = %+v", res)
	}
	files := openZip(t, dest)
	errData, ok := files["records/error.jsonl"]
	if !ok {
		t.Fatalf("zip lacks records/error.jsonl: %v", zipKeys(files))
	}
	if !strings.Contains(string(errData), `"operation_id":"op-export-1"`) {
		t.Fatalf("primary error records lack the operation chain:\n%s", errData)
	}
	ctxData, ok := files["context/operation-context.jsonl"]
	if !ok {
		t.Fatalf("zip lacks operation context: %v", zipKeys(files))
	}
	if !strings.Contains(string(ctxData), `"event_type":"operation_start"`) {
		t.Fatalf("context lacks the operation chain:\n%s", ctxData)
	}
	readme, ok := files["README.md"]
	if !ok || !strings.Contains(string(readme), "记录导出") {
		t.Fatal("zip lacks a proper README.md")
	}
	var manifest map[string]any
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if manifest["category"] != "error" {
		t.Fatalf("manifest category = %v", manifest["category"])
	}
	primary, _ := manifest["primaryCounts"].(map[string]any)
	context, _ := manifest["contextCounts"].(map[string]any)
	if primary["error"] != float64(1) {
		t.Fatalf("primaryCounts = %v", primary)
	}
	if context["operation"] != float64(3) { // start + step + result
		t.Fatalf("contextCounts = %v", context)
	}
	if manifest["actualEarliest"] == nil || manifest["actualLatest"] == nil {
		t.Fatalf("manifest lacks actual range: %v", manifest)
	}
	// Every file entry must carry a SHA-256.
	for _, fe := range manifest["files"].([]any) {
		m := fe.(map[string]any)
		if m["sha256"] == "" || m["sha256"] == nil {
			t.Fatalf("file entry lacks sha256: %v", m)
		}
	}
}

func TestExportRecordsDateFilterAndAll(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.RecordEvent(logging.Event{
		EventType: logging.EventTypeRuntime, Category: logging.EventCategoryRuntime,
		Module: "测试", Action: "old", Timestamp: "2026-10-20T08:00:00+08:00",
	})
	f.logs.RecordEvent(logging.Event{
		EventType: logging.EventTypeRuntime, Category: logging.EventCategoryRuntime,
		Module: "测试", Action: "new", Timestamp: "2026-11-15T09:00:00+08:00",
	})
	dest := filepath.Join(t.TempDir(), "out.zip")
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	if _, err := f.svc.ExportRecords("", "runtime", "2026-11-01", "2026-11-30"); err != nil {
		t.Fatalf("ExportRecords: %v", err)
	}
	files := openZip(t, dest)
	data := string(files["records/runtime.jsonl"])
	if !strings.Contains(data, `"action":"new"`) || strings.Contains(data, `"action":"old"`) {
		t.Fatalf("date filter wrong:\n%s", data)
	}
	// Invalid range is an explicit error.
	if _, err := f.svc.ExportRecords("", "runtime", "2026-12-01", "2026-11-01"); err == nil {
		t.Fatal("reversed range accepted")
	}
}

func TestExportRecordsEmptyCategory(t *testing.T) {
	f := newDiagFixture(t)
	// Before any export, the category is genuinely empty.
	info, err := f.svc.GetLogExportInfo("operation")
	if err != nil {
		t.Fatal(err)
	}
	if info.HasRecords {
		t.Fatal("fresh operation category should be empty")
	}
	dest := filepath.Join(t.TempDir(), "out.zip")
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	res, err := f.svc.ExportRecords("", "operation", "", "")
	if err != nil {
		t.Fatalf("ExportRecords empty: %v", err)
	}
	files := openZip(t, dest)
	if files["manifest.json"] == nil || files["README.md"] == nil {
		t.Fatalf("empty export lacks manifest/README: %v", zipKeys(files))
	}
	// The only records in the export are the export operation's
	// own in-flight events (start + step): recording begins before
	// collection, so an export can never silently omit its own
	// audit trail.
	data, ok := files["records/operation.jsonl"]
	if !ok {
		t.Fatalf("export lacks its own start record: %v", zipKeys(files))
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !strings.Contains(lines[0], `"action":"logs.export"`) ||
		!strings.Contains(lines[0], `"event_type":"operation_start"`) {
		t.Fatalf("unexpected export content:\n%s", data)
	}
	for _, ln := range lines {
		if !strings.Contains(ln, `"operation_id":"`) {
			t.Fatalf("export record without operation_id:\n%s", ln)
		}
	}
	if res.Files != 1 {
		t.Fatalf("export files = %d, want 1 (the records file)", res.Files)
	}
}

func TestExportRecordsSensitivityFailClosed(t *testing.T) {
	f := newDiagFixture(t)
	// A poisoned line (credential-shaped) in the operation stream
	// must abort the export instead of shipping.
	opFiles := f.logs.Files(logging.CategoryOperation)
	if len(opFiles) == 0 {
		t.Fatal("no operation file")
	}
	fh, err := os.OpenFile(opFiles[0].Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("{\"timestamp\":\"2026-11-15T10:00:00Z\",\"secret_ref\":\"vault:x\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()
	dest := filepath.Join(t.TempDir(), "out.zip")
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	if _, err := f.svc.ExportRecords("", "operation", "", ""); err == nil {
		t.Fatal("export shipped credential-shaped content")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("failed export left a file behind")
	}
}

func TestSetLogCapacityPersistsAndApplies(t *testing.T) {
	f := newDiagFixture(t)
	view, err := f.svc.SetLogCapacity("", 50<<20)
	if err != nil {
		t.Fatalf("SetLogCapacity: %v", err)
	}
	if view.LimitBytes != 50<<20 {
		t.Fatalf("view limit = %d", view.LimitBytes)
	}
	if got := f.logs.CapacityLimit(); got != 50<<20 {
		t.Fatalf("system limit = %d", got)
	}
	// Persisted in settings.json: a fresh load sees it.
	loaded, err := settings.Load(f.layout.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LogCapacityBytes != 50<<20 {
		t.Fatalf("persisted capacity = %d", loaded.LogCapacityBytes)
	}
	if got := f.svc.app.EffectiveLogCapacityBytes(); got != 50<<20 {
		t.Fatalf("effective capacity = %d", got)
	}
	// Out-of-range values are rejected.
	if _, err := f.svc.SetLogCapacity("", 100); err == nil {
		t.Fatal("tiny capacity accepted")
	}
	// The change itself is recorded as an operation.
	info, err := f.svc.GetLogExportInfo("operation")
	if err != nil {
		t.Fatal(err)
	}
	if !info.HasRecords {
		t.Fatal("capacity change was not recorded")
	}
}

func TestRecordClientEvent(t *testing.T) {
	f := newDiagFixture(t)
	if err := f.svc.RecordClientEvent(ClientEventInput{
		Category: "runtime", Module: "界面", Action: "打开页面",
		Status: "info", Metadata: map[string]string{"page": "instances"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RecordClientEvent(ClientEventInput{Category: "bogus"}); err == nil {
		t.Fatal("bogus category accepted")
	}
	_, _, count, _, ok := f.logs.DateRange(logging.EventCategoryRuntime)
	if !ok || count == 0 {
		t.Fatal("client runtime event not recorded")
	}
}

func zipKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExportRecordsReplaceFailureKeepsExistingDest preserves the
// atomic-replace guarantee for the retained record exports: when
// the platform replace fails, a previous export at the chosen
// destination stays byte-identical and no partial file appears.
func TestExportRecordsReplaceFailureKeepsExistingDest(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.Logger().Info("hello")
	dest := filepath.Join(t.TempDir(), "out.zip")
	if err := os.WriteFile(dest, []byte("previous export"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	f.svc.replaceFile = func(src, dst string) error { return errString("replace denied") }
	if _, err := f.svc.ExportRecords("", "all", "", ""); err == nil {
		t.Fatal("export with failing replace must error")
	}
	body, err := os.ReadFile(dest)
	if err != nil || string(body) != "previous export" {
		t.Fatalf("existing destination changed: %q, %v", body, err)
	}
}

func TestExportRecordsPickerFailureIsExplicit(t *testing.T) {
	f := newDiagFixture(t)
	f.svc.SavePathPicker = func(string) (string, error) { return "", errString("dialog boom") }
	if _, err := f.svc.ExportRecords("", "all", "", ""); err == nil {
		t.Fatal("picker failure was hidden")
	}
}

// Logging degradation: when the recording system is broken
// (here: already closed), business operations must still succeed
// — the failure is counted, never propagated.
func TestBusinessOperationSurvivesBrokenRecording(t *testing.T) {
	instSvc, cfg := testInstanceService(t)
	logs, err := logging.NewSystem(cfg)
	if err != nil {
		t.Fatal(err)
	}
	instSvc.Auditor = logs
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	inst, err := instSvc.CreateInstance("", CreateInstanceInput{Name: "降级实例"})
	if err != nil {
		t.Fatalf("business operation failed because of logging: %v", err)
	}
	if inst.ID == "" {
		t.Fatal("instance not created")
	}
	if logs.WriteErrors() == 0 {
		t.Fatal("recording failures were not counted")
	}
}
