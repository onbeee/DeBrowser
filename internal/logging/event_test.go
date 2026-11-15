package logging

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// readStreamLines returns the parsed records of one stream.
func readStreamLines(t *testing.T, sys *System, cat Category) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range sys.Files(cat) {
		data, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("stream %s has an unparseable line %q: %v", cat, line, err)
			}
			out = append(out, rec)
		}
	}
	return out
}

func TestOperationChainFull(t *testing.T) {
	sys, _ := testSystem(t)
	op := sys.BeginOperation("op-fixed-2", "浏览器实例", "instance.create", "instance", "", "user")
	op.Step("校验输入", StatusSuccess, nil)
	op.Succeed(nil)
	rootID := op.RootEventID()
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readStreamLines(t, sys, CategoryOperation)
	if len(recs) != 3 {
		t.Fatalf("operation records = %d, want 3 (start, step, result)", len(recs))
	}
	byType := map[string]map[string]any{}
	for _, r := range recs {
		if r["operation_id"] != "op-fixed-2" {
			t.Fatalf("record without the shared operation_id: %v", r)
		}
		if r["category"] != "operation" {
			t.Fatalf("record category = %v", r["category"])
		}
		byType[r["event_type"].(string)] = r
	}
	start := byType["operation_start"]
	if start == nil || start["event_id"] != rootID || start["status"] != "started" {
		t.Fatalf("bad start record: %v", start)
	}
	step := byType["operation_step"]
	if step == nil || step["parent_event_id"] != rootID {
		t.Fatalf("step not parented to root: %v", step)
	}
	result := byType["operation_result"]
	if result == nil || result["status"] != "success" || result["parent_event_id"] != step["event_id"] {
		t.Fatalf("result not parented to last step: %v", result)
	}
	if _, ok := result["duration_ms"]; !ok {
		t.Fatalf("result lacks duration_ms: %v", result)
	}
	// Nothing leaked into the other streams: each event is stored
	// exactly once, in its own category.
	if got := readStreamLines(t, sys, CategoryError); len(got) != 0 {
		t.Fatalf("error stream has %d records, want 0", len(got))
	}
}

func TestOperationFailureWritesErrorRecord(t *testing.T) {
	sys, _ := testSystem(t)
	op := sys.BeginOperation("", "浏览器内核", "kernel.install", "kernel", "", "user")
	op.Step("下载、解压与安装验证", StatusFailed, nil)
	op.Fail(errTest("安装内核失败：签名验证未通过"), "operation_failed", "failed", "安装内核失败：签名验证未通过")
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}
	errs := readStreamLines(t, sys, CategoryError)
	if len(errs) != 1 {
		t.Fatalf("error records = %d, want 1", len(errs))
	}
	e := errs[0]
	for key, want := range map[string]string{
		"category":       "error",
		"event_type":     "error",
		"module":         "浏览器内核",
		"action":         "kernel.install",
		"error_code":     "operation_failed",
		"error_category": "failed",
		"user_message":   "安装内核失败：签名验证未通过",
		"status":         "failed",
	} {
		if e[key] != want {
			t.Fatalf("error record %s = %v, want %q (record: %v)", key, e[key], want, e)
		}
	}
	if e["operation_id"] == "" || e["operation_id"] != op.ID() {
		t.Fatalf("error record not linked to the operation: %v", e)
	}
	if e["error_detail"] == "" {
		t.Fatalf("error record lacks detail: %v", e)
	}
	if _, ok := e["duration_ms"]; !ok {
		t.Fatalf("error record lacks duration_ms: %v", e)
	}
	ops := readStreamLines(t, sys, CategoryOperation)
	found := false
	for _, r := range ops {
		if r["event_type"] == "operation_result" && r["status"] == "failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("operation stream lacks the failed result: %v", ops)
	}
}

func TestRuntimeEventsGoToRunStream(t *testing.T) {
	sys, _ := testSystem(t)
	sys.RecordLifecycle("应用启动", StatusStarted, nil)
	sys.RecordRuntime("界面", "打开页面", StatusInfo, map[string]string{"page": "instances"})
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readStreamLines(t, sys, CategoryRun)
	found := 0
	for _, r := range recs {
		if r["category"] == "runtime" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("runtime records in run stream = %d, want 2 (%v)", found, recs)
	}
	if got := readStreamLines(t, sys, CategoryOperation); len(got) != 0 {
		t.Fatalf("operation stream polluted: %v", got)
	}
}

func TestEventSanitization(t *testing.T) {
	sys, _ := testSystem(t)
	sys.RecordEvent(Event{
		EventType:   EventTypeError,
		Category:    EventCategoryError,
		Module:      "测试",
		Action:      "test.fail",
		ErrorDetail: "dial https://user:pw@example.com/x failed, token=abc123",
		Metadata: map[string]string{
			"token":   "should-be-dropped",
			"attempt": "3",
		},
	})
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readStreamLines(t, sys, CategoryError)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	raw, _ := json.Marshal(recs[0])
	for _, forbidden := range []string{"user:pw", "abc123", "should-be-dropped"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("event leaked %q: %s", forbidden, raw)
		}
	}
	md, _ := recs[0]["metadata"].(map[string]any)
	if md["attempt"] != "3" {
		t.Fatalf("safe metadata lost: %v", recs[0])
	}
}

func TestConcurrentOperations(t *testing.T) {
	sys, _ := testSystem(t)
	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := sys.BeginOperation("", "测试", "test.op", "", "", "user")
			op.Step("步骤", StatusSuccess, nil)
			op.Succeed(nil)
		}()
	}
	wg.Wait()
	if err := sys.Close(); err != nil {
		t.Fatal(err)
	}
	recs := readStreamLines(t, sys, CategoryOperation)
	if len(recs) != workers*3 {
		t.Fatalf("records = %d, want %d", len(recs), workers*3)
	}
	ids := map[string]bool{}
	ops := map[string]int{}
	for _, r := range recs {
		if ids[r["event_id"].(string)] {
			t.Fatalf("duplicate event_id %v", r["event_id"])
		}
		ids[r["event_id"].(string)] = true
		ops[r["operation_id"].(string)]++
	}
	if len(ops) != workers {
		t.Fatalf("distinct operation ids = %d, want %d", len(ops), workers)
	}
	for id, n := range ops {
		if n != 3 {
			t.Fatalf("operation %s has %d records, want 3", id, n)
		}
	}
}

func TestNilSystemRecordingIsSafe(t *testing.T) {
	var sys *System
	sys.RecordEvent(Event{Category: EventCategoryOperation})
	op := sys.BeginOperation("", "m", "a", "", "", "")
	op.Step("s", StatusSuccess, nil)
	op.Succeed(nil)
	op.Fail(errTest("x"), "c", "failed", "x")
	sys.RecordError("m", "a", errTest("x"), "c", "failed", "x")
	if sys.WriteErrors() != 0 {
		t.Fatal("nil system reported write errors")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
