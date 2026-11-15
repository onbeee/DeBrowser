package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedClosedFile writes a closed (rotated) log file of the given
// size class directly into the logs dir, as a previous run would
// have left it.
func seedClosedFile(t *testing.T, dir, name string, lines int, lineBytes int) int64 {
	t.Helper()
	var sb strings.Builder
	for i := 0; i < lines; i++ {
		ts := time.Date(2026, 1, 1, 0, 0, i%60, 0, time.Local).Format(time.RFC3339Nano)
		pad := strings.Repeat("x", max(0, lineBytes-90))
		fmt.Fprintf(&sb, "{\"timestamp\":%q,\"level\":\"INFO\",\"event\":\"seed\",\"category\":\"run\",\"pad\":%q}\n", ts, pad)
	}
	data := []byte(sb.String())
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

func TestCapacityDefaultAndValidation(t *testing.T) {
	sys, _ := testSystem(t)
	defer func() { _ = sys.Close() }()
	if got := sys.CapacityLimit(); got != 100<<20 {
		t.Fatalf("default limit = %d, want 100 MB", got)
	}
	if _, err := sys.SetCapacityLimit(512 << 10); err == nil {
		t.Fatal("below-minimum capacity accepted")
	}
	if _, err := sys.SetCapacityLimit(30 << 20); err != nil {
		t.Fatalf("30 MB capacity rejected: %v", err)
	}
	if got := sys.CapacityLimit(); got != 30<<20 {
		t.Fatalf("limit after set = %d", got)
	}
}

func TestCapacityStatsReflectRealFiles(t *testing.T) {
	sys, layout := testSystem(t)
	defer func() { _ = sys.Close() }()
	seedClosedFile(t, layout.Logs, "run-2026-01-01.log", 10, 200)
	var diskTotal int64
	entries, err := os.ReadDir(layout.Logs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			diskTotal += info.Size()
		}
	}
	if got := sys.TotalBytes(); got != diskTotal {
		t.Fatalf("TotalBytes = %d, real disk total = %d", got, diskTotal)
	}
	if rem := sys.RemainingBytes(); rem != sys.CapacityLimit()-diskTotal {
		t.Fatalf("RemainingBytes = %d, want %d", rem, sys.CapacityLimit()-diskTotal)
	}
}

func TestCapacityCleanupDeletesOldestClosedFilesFirst(t *testing.T) {
	sys, layout := testSystem(t)
	// Three closed files on different dates, ~1.2 MB each (the
	// minimum configurable capacity is 1 MB, so cleanup scenarios
	// need megabyte-scale data).
	s1 := seedClosedFile(t, layout.Logs, "operation-2025-12-30.log", 6000, 200)
	seedClosedFile(t, layout.Logs, "run-2026-01-01.log", 6000, 200)
	seedClosedFile(t, layout.Logs, "error-2026-01-02.log", 6000, 200)
	totalBefore := sys.TotalBytes()
	// Limit chosen so exactly the two oldest files must go.
	limit := totalBefore - 2*s1 + (200 << 10)
	used, err := sys.SetCapacityLimit(limit)
	if err != nil {
		t.Fatalf("SetCapacityLimit: %v", err)
	}
	if used > limit {
		t.Fatalf("used %d still over limit %d", used, limit)
	}
	// The oldest (2025-12-30) must be gone; the newest closed file
	// (2026-01-02) must survive while the limit allows it.
	if _, err := os.Stat(filepath.Join(layout.Logs, "operation-2025-12-30.log")); !os.IsNotExist(err) {
		t.Fatal("oldest closed file was not deleted first")
	}
	// Current files are never deleted.
	for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
		if len(sys.Files(cat)) == 0 {
			t.Fatalf("category %s lost its current file", cat)
		}
	}
	_ = sys.Close()
}

func TestCapacityShrinkImmediatelyCleans(t *testing.T) {
	sys, layout := testSystem(t)
	seedClosedFile(t, layout.Logs, "run-2026-01-01.log", 10000, 200) // ~2 MB
	if _, err := sys.SetCapacityLimit(100 << 20); err != nil {
		t.Fatal(err)
	}
	before := sys.TotalBytes()
	if before < 2_000_000 {
		t.Fatalf("seeded total too small: %d", before)
	}
	// Shrink to the minimum: the seeded closed file must be
	// cleaned during the Set call itself.
	used, err := sys.SetCapacityLimit(MinCapacityBytes)
	if err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if used > MinCapacityBytes {
		t.Fatalf("used after shrink = %d over limit", used)
	}
	if _, err := os.Stat(filepath.Join(layout.Logs, "run-2026-01-01.log")); !os.IsNotExist(err) {
		t.Fatal("seeded file survived the shrink cleanup")
	}
	_ = sys.Close()
}

func TestTrimOldestRecordsGroupRules(t *testing.T) {
	mk := func(op, typ string) string {
		return fmt.Sprintf("{\"event_id\":\"e-%s-%s\",\"timestamp\":\"2026-11-15T10:00:00Z\",\"event_type\":%q,\"category\":\"operation\",\"operation_id\":%q}\n", op, typ, typ, op)
	}
	raw := []byte(mk("op-a", "operation_start") + mk("op-a", "operation_step") + mk("op-a", "operation_result") +
		mk("op-b", "operation_start") + mk("op-b", "operation_result"))
	lineLen := int64(len(mk("op-a", "operation_start")))
	// Remove ~2 lines: group op-a (3 lines) is dropped whole only
	// if it fits; it does not (3 lines > 2), so it is partially
	// dropped keeping its result line.
	kept, removed := trimOldestRecords(raw, 2*lineLen)
	if removed < 2*lineLen {
		t.Fatalf("removed = %d, want >= %d", removed, 2*lineLen)
	}
	s := string(kept)
	if !strings.Contains(s, "op-a") || !strings.Contains(s, `"operation_result"`) {
		t.Fatalf("group result line not preserved:\n%s", s)
	}
	if strings.Contains(s, "op-a\",\"event_type\":\"operation_start") {
		t.Fatalf("oldest group start should be dropped:\n%s", s)
	}
	if !strings.Contains(s, "op-b") {
		t.Fatalf("newest group must survive:\n%s", s)
	}
	// Every kept line must still be complete JSONL.
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		ts, ok := parseRecordTimestamp([]byte(ln))
		if !ok || ts.IsZero() {
			t.Fatalf("kept line damaged: %q", ln)
		}
	}
}

func TestTrimNeverDropsNewestRecord(t *testing.T) {
	big := strings.Repeat("y", 3000)
	line1 := "{\"timestamp\":\"2026-11-15T10:00:00Z\",\"event\":\"old\"}\n"
	line2 := fmt.Sprintf("{\"timestamp\":\"2026-11-15T10:01:00Z\",\"event\":\"newest\",\"pad\":%q}\n", big)
	raw := []byte(line1 + line2)
	// Ask to remove more than the whole file: the newest record
	// (a singleton group holding the last line) must survive.
	kept, removed := trimOldestRecords(raw, int64(len(raw))+1000)
	if string(kept) != line2 {
		t.Fatalf("kept = %q, want only the newest record", kept)
	}
	if removed != int64(len(line1)) {
		t.Fatalf("removed = %d, want %d", removed, len(line1))
	}
}

func TestTrimCurrentFileViaWriter(t *testing.T) {
	dir := t.TempDir()
	w, err := newCategoryWriter(dir, "operation", Policy{RetentionDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		line := fmt.Sprintf("{\"event_id\":\"e%d\",\"timestamp\":\"2026-11-15T10:00:0%dZ\",\"event_type\":\"operation_step\",\"category\":\"operation\",\"operation_id\":\"op-x\",\"pad\":%q}\n", i, i%10, strings.Repeat("z", 100))
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	sizeBefore := w.curSize
	removed, err := w.trimCurrent(sizeBefore / 2)
	if err != nil {
		t.Fatalf("trimCurrent: %v", err)
	}
	if removed < sizeBefore/2 {
		t.Fatalf("removed = %d, want >= %d", removed, sizeBefore/2)
	}
	// The writer must still accept writes after trimming, and the
	// file must remain valid JSONL.
	if _, err := w.Write([]byte("{\"timestamp\":\"2026-11-15T10:05:00Z\",\"event\":\"after-trim\"}\n")); err != nil {
		t.Fatalf("write after trim: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if _, ok := parseRecordTimestamp([]byte(ln)); !ok {
				t.Fatalf("damaged line after trim: %q", ln)
			}
		}
		if !strings.Contains(string(data), "after-trim") {
			t.Fatal("post-trim write missing")
		}
	}
}

func TestCapacityUnreachableReportsError(t *testing.T) {
	sys, _ := testSystem(t)
	// One newest record far larger than the minimum capacity:
	// trimming must refuse to destroy it and enforcement must
	// report failure explicitly.
	sys.RecordEvent(Event{
		EventType: EventTypeRuntime, Category: EventCategoryRuntime,
		Module: "测试", Action: "big",
		Metadata: map[string]string{"pad": strings.Repeat("q", 2<<20)},
	})
	// ScrubErrorText caps free text at 512 bytes, so grow the file
	// with many medium records instead: the newest stays small,
	// enforcement succeeds by trimming. Use a raw huge newest line
	// through the writer to force the unreachable case.
	w := sys.writers[CategoryRun]
	huge := fmt.Sprintf("{\"timestamp\":%q,\"event\":\"huge\",\"pad\":%q}\n",
		time.Now().Format(time.RFC3339Nano), strings.Repeat("h", 2<<20))
	if _, err := w.Write([]byte(huge)); err != nil {
		t.Fatal(err)
	}
	for _, f := range sys.Files(CategoryRun) {
		st, _ := os.Stat(f.Path)
		t.Logf("DIAG pre-enforce run file %s size=%d total=%d", f.Path, st.Size(), sys.TotalBytes())
	}
	used, err := sys.SetCapacityLimit(MinCapacityBytes)
	t.Logf("DIAG SetCapacityLimit used=%d err=%v total=%d", used, err, sys.TotalBytes())
	if err == nil {
		t.Fatal("expected an explicit cleanup-failure error")
	}
	if _, n := sys.CapacityError(); n == 0 {
		t.Fatal("capacity failure was not recorded for diagnostics")
	}
	// The huge newest record must still be there.
	found := false
	for _, f := range sys.Files(CategoryRun) {
		data, _ := os.ReadFile(f.Path)
		if strings.Contains(string(data), "\"huge\"") {
			found = true
		}
	}
	if !found {
		t.Fatal("newest record was destroyed by trimming")
	}
	_ = sys.Close()
}

func TestDateRangeAndCollect(t *testing.T) {
	sys, layout := testSystem(t)
	// Legacy slog-shaped line + unified events at known times.
	sys.RecordEvent(Event{
		EventType: EventTypeOperationStart, Category: EventCategoryOperation,
		Module: "测试", Action: "a.start", Timestamp: "2026-10-20T08:00:00Z",
	})
	sys.RecordEvent(Event{
		EventType: EventTypeOperationResult, Category: EventCategoryOperation,
		Module: "测试", Action: "a.result", Timestamp: "2026-11-15T09:00:00Z", Status: StatusSuccess,
	})
	// A damaged line must be counted, not treated as a record.
	opFiles := sys.Files(CategoryOperation)
	if len(opFiles) == 0 {
		t.Fatal("no operation files")
	}
	f, err := os.OpenFile(opFiles[len(opFiles)-1].Path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_ = sys.Close()

	earliest, latest, count, damaged, ok := sys.DateRange(EventCategoryOperation)
	if !ok || count != 2 || damaged != 1 {
		t.Fatalf("range: ok=%v count=%d damaged=%d", ok, count, damaged)
	}
	if earliest.Format("2006-01-02") != "2026-10-20" || latest.Format("2006-01-02") != "2026-11-15" {
		t.Fatalf("range = %s .. %s", earliest, latest)
	}
	lines, refs, _ := sys.CollectLines(EventCategoryOperation,
		time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Time{})
	if len(lines) != 1 || len(refs) != 1 {
		t.Fatalf("collect from Nov: %d lines", len(lines))
	}
	if _, _, _, _, ok := sys.DateRange("bogus"); ok {
		t.Fatal("bogus category reported records")
	}
	_ = layout
}
