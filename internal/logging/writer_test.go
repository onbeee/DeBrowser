package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testWriter(t *testing.T, policy Policy) (*categoryWriter, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := newCategoryWriter(dir, "run", policy)
	if err != nil {
		t.Fatalf("newCategoryWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

func plantFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWriterDatedFileAndJSONLines(t *testing.T) {
	w, dir := testWriter(t, Policy{RetentionDays: 30, CapBytes: 20 << 20})
	if _, err := w.Write([]byte("{\"a\":1}\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := "run-" + time.Now().Format("2006-01-02") + ".log"
	data, err := os.ReadFile(filepath.Join(dir, want))
	if err != nil {
		t.Fatalf("expected dated file %s: %v", want, err)
	}
	if string(data) != "{\"a\":1}\n" {
		t.Fatalf("content = %q", data)
	}
}

func TestWriterRollsAtFiveMB(t *testing.T) {
	w, dir := testWriter(t, Policy{RetentionDays: 30, CapBytes: 100 << 20})
	chunk := strings.Repeat("y", 1<<20) // 1 MB per write
	for i := 0; i < 6; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	first := filepath.Join(dir, "run-"+today+".log")
	second := filepath.Join(dir, "run-"+today+".002.log")
	st1, err := os.Stat(first)
	if err != nil {
		t.Fatalf("volume 1 missing: %v", err)
	}
	if st1.Size() > maxFileBytes {
		t.Fatalf("volume 1 size = %d, exceeds %d", st1.Size(), maxFileBytes)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("volume 2 missing after roll: %v", err)
	}
}

func TestWriterRetentionDeletesOldFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := "run-" + now.AddDate(0, 0, -45).Format("2006-01-02") + ".log"
	recent := "run-" + now.AddDate(0, 0, -10).Format("2006-01-02") + ".log"
	plantFile(t, dir, old, 10)
	plantFile(t, dir, recent, 10)
	// Another category's file must never be touched.
	plantFile(t, dir, "operation-"+now.AddDate(0, 0, -45).Format("2006-01-02")+".log", 10)
	w, err := newCategoryWriter(dir, "run", Policy{RetentionDays: 30, CapBytes: 20 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Fatalf("45-day-old run file still exists (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, recent)); err != nil {
		t.Fatalf("10-day-old run file was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "operation-"+now.AddDate(0, 0, -45).Format("2006-01-02")+".log")); err != nil {
		t.Fatalf("another category's file was deleted: %v", err)
	}
}

func TestWriterRetentionNinetyDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	d100 := "error-" + now.AddDate(0, 0, -100).Format("2006-01-02") + ".log"
	d45 := "error-" + now.AddDate(0, 0, -45).Format("2006-01-02") + ".log"
	plantFile(t, dir, d100, 10)
	plantFile(t, dir, d45, 10)
	w, err := newCategoryWriter(dir, "error", Policy{RetentionDays: 90, CapBytes: 50 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := os.Stat(filepath.Join(dir, d100)); !os.IsNotExist(err) {
		t.Fatalf("100-day-old file still exists under a 90-day policy")
	}
	if _, err := os.Stat(filepath.Join(dir, d45)); err != nil {
		t.Fatalf("45-day-old file deleted under a 90-day policy: %v", err)
	}
}

func TestWriterCapacityDeletesOldestButNeverCurrent(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	d1 := "run-" + now.AddDate(0, 0, -3).Format("2006-01-02") + ".log"
	d2 := "run-" + now.AddDate(0, 0, -2).Format("2006-01-02") + ".log"
	d3 := "run-" + now.AddDate(0, 0, -1).Format("2006-01-02") + ".log"
	plantFile(t, dir, d1, 300)
	plantFile(t, dir, d2, 300)
	plantFile(t, dir, d3, 300)
	// Cap 700: after open (current file created), oldest files go.
	w, err := newCategoryWriter(dir, "run", Policy{RetentionDays: 90, CapBytes: 700})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("current\n")); err != nil {
		t.Fatal(err)
	}
	files, _, _ := w.stats()
	var total int64
	for _, f := range files {
		total += f.size
	}
	if total > 700 {
		t.Fatalf("total = %d, exceeds cap 700 (files: %v)", total, files)
	}
	if _, err := os.Stat(filepath.Join(dir, d1)); !os.IsNotExist(err) {
		t.Fatalf("oldest file not deleted first")
	}
	cur := w.curName
	if _, err := os.Stat(filepath.Join(dir, cur)); err != nil {
		t.Fatalf("current file was deleted: %v", err)
	}
	_ = w.Close()
}

func TestWriterConcurrentWrites(t *testing.T) {
	w, dir := testWriter(t, Policy{RetentionDays: 30, CapBytes: 100 << 20})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				line := fmt.Sprintf("{\"g\":%d,\"i\":%d}\n", g, i)
				if _, err := w.Write([]byte(line)); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "run-*.log"))
	var lines int
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines += strings.Count(string(data), "\n")
	}
	if lines != 800 {
		t.Fatalf("lines = %d, want 800 (interleaved or lost writes)", lines)
	}
}

func TestWriterDateRollover(t *testing.T) {
	dir := t.TempDir()
	day1 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)
	day2 := day1.AddDate(0, 0, 1)
	clock := day1
	w2, err := newCategoryWriterAt(dir, "run", Policy{RetentionDays: 30, CapBytes: 20 << 20},
		func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w2.Write([]byte("day1\n")); err != nil {
		t.Fatal(err)
	}
	clock = day2
	if _, err := w2.Write([]byte("day2\n")); err != nil {
		t.Fatal(err)
	}
	_ = w2.Close()
	d1, err := os.ReadFile(filepath.Join(dir, "run-2026-11-12.log"))
	if err != nil || string(d1) != "day1\n" {
		t.Fatalf("day1 file = %q, err %v", d1, err)
	}
	d2, err := os.ReadFile(filepath.Join(dir, "run-2026-11-13.log"))
	if err != nil || string(d2) != "day2\n" {
		t.Fatalf("day2 file = %q, err %v", d2, err)
	}
}
