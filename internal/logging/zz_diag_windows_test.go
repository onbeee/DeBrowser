package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestZZDiagWindows(t *testing.T) {
	sys, layout := testSystem(t)
	dump := func(stage string) {
		entries, _ := os.ReadDir(layout.Logs)
		for _, e := range entries {
			st, _ := os.Stat(filepath.Join(layout.Logs, e.Name()))
			t.Logf("%s file=%s size=%d", stage, e.Name(), st.Size())
		}
		t.Logf("%s TotalBytes=%d limit=%d", stage, sys.TotalBytes(), sys.CapacityLimit())
		for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
			t.Logf("%s writer %s current=%q", stage, cat, sys.writers[cat].currentName())
		}
	}
	sys.RecordEvent(Event{
		EventType: EventTypeRuntime, Category: EventCategoryRuntime,
		Module: "测试", Action: "big",
		Metadata: map[string]string{"pad": strings.Repeat("q", 2<<20)},
	})
	dump("after-q")
	w := sys.writers[CategoryRun]
	huge := fmt.Sprintf("{\"timestamp\":%q,\"event\":\"huge\",\"pad\":%q}\n",
		time.Now().Format(time.RFC3339Nano), strings.Repeat("h", 2<<20))
	n, err := w.Write([]byte(huge))
	t.Logf("h write n=%d err=%v len=%d", n, err, len(huge))
	dump("after-h")
	used, serr := sys.SetCapacityLimit(MinCapacityBytes)
	t.Logf("SetCapacityLimit used=%d err=%v", used, serr)
	dump("after-enforce")
	for _, f := range sys.Files(CategoryRun) {
		data, _ := os.ReadFile(f.Path)
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		sizes := []string{}
		for _, ln := range lines {
			sizes = append(sizes, fmt.Sprint(len(ln)))
		}
		t.Logf("run file %s lines=%v", f.Path, sizes)
	}
	t.Fatal("diag end")
}
