package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Policy is the retention rule for one log category: files older
// than RetentionDays are deleted, and the category's total size is
// capped at CapBytes (oldest files are deleted first; the file
// currently being written is never deleted).
type Policy struct {
	RetentionDays int
	CapBytes      int64
}

// maxFileBytes is the per-file volume limit: when the current file
// would grow past it, the writer rolls over to the next volume.
const maxFileBytes = 5 << 20 // 5 MB

// fileEntry describes one log file of a category.
type fileEntry struct {
	name string // base name, e.g. run-2026-11-13.002.log
	date time.Time
	vol  int
	size int64
}

// categoryWriter appends JSON Lines to the dated files of one log
// category inside the logs directory. Files are named
// <prefix>-YYYY-MM-DD.log with additional volumes
// <prefix>-YYYY-MM-DD.002.log, .003.log, ... once a file reaches
// maxFileBytes. Retention and capacity enforcement run when the
// writer opens, on every roll-over, and periodically while writing;
// enforcement only ever touches files of this category, never the
// currently open file, and never anything outside the logs
// directory. All methods are safe for concurrent use.
type categoryWriter struct {
	dir    string
	prefix string
	policy Policy
	now    func() time.Time // injectable for tests

	mu          sync.Mutex
	file        *os.File
	curName     string
	curDate     time.Time
	curSize     int64
	closed      bool
	writes      int
	cleanupErr  error // most recent enforcement failure, for diagnostics
	cleanupErrN int
}

func newCategoryWriter(dir, prefix string, policy Policy) (*categoryWriter, error) {
	return newCategoryWriterAt(dir, prefix, policy, time.Now)
}

func newCategoryWriterAt(dir, prefix string, policy Policy, now func() time.Time) (*categoryWriter, error) {
	w := &categoryWriter{dir: dir, prefix: prefix, policy: policy, now: now}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("logging: create log directory: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Open the current file first so enforcement can never mistake
	// it for a deletable old file.
	if err := w.openCurrentLocked(); err != nil {
		return nil, err
	}
	w.enforceLocked()
	return w, nil
}

// filePattern matches exactly the files this category owns.
func (w *categoryWriter) filePattern() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(w.prefix) + `-(\d{4}-\d{2}-\d{2})(?:\.(\d{3}))?\.log$`)
}

func (w *categoryWriter) scanLocked() []fileEntry {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil
	}
	pat := w.filePattern()
	var out []fileEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := pat.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", m[1], time.Local)
		if err != nil {
			continue
		}
		vol := 1
		if m[2] != "" {
			fmt.Sscanf(m[2], "%d", &vol)
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileEntry{name: e.Name(), date: date, vol: vol, size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].date.Equal(out[j].date) {
			return out[i].date.Before(out[j].date)
		}
		return out[i].vol < out[j].vol
	})
	return out
}

func (w *categoryWriter) nameFor(date time.Time, vol int) string {
	if vol <= 1 {
		return fmt.Sprintf("%s-%s.log", w.prefix, date.Format("2006-01-02"))
	}
	return fmt.Sprintf("%s-%s.%03d.log", w.prefix, date.Format("2006-01-02"), vol)
}

// openCurrentLocked opens today's latest volume for appending (or
// volume 1 when none exists yet).
func (w *categoryWriter) openCurrentLocked() error {
	today := dayOf(w.now())
	var target fileEntry
	found := false
	for _, f := range w.scanLocked() {
		if dayOf(f.date).Equal(today) {
			target, found = f, true // scan is sorted: last match is the newest volume
		}
	}
	name := w.nameFor(today, 1)
	var size int64
	if found {
		name, size = target.name, target.size
	}
	fh, err := os.OpenFile(filepath.Join(w.dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("logging: open log file %q: %w", name, err)
	}
	w.file, w.curName, w.curDate, w.curSize = fh, name, today, size
	return nil
}

// rollLocked closes the current file and opens the next one: the
// next volume of today, or volume 1 of the new day. The closed file
// becomes eligible for enforcement; the new current file never is.
func (w *categoryWriter) rollLocked() error {
	today := dayOf(w.now())
	_ = w.file.Close()
	w.file = nil
	// Determine the next volume for today from what is on disk.
	vol := 1
	for _, f := range w.scanLocked() {
		if dayOf(f.date).Equal(today) && f.vol >= vol {
			vol = f.vol + 1
		}
	}
	// If the current file was today's volume 1 and it is still
	// empty, reopen it instead of littering volumes.
	name := w.nameFor(today, vol)
	fh, err := os.OpenFile(filepath.Join(w.dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("logging: roll log file %q: %w", name, err)
	}
	st, err := fh.Stat()
	if err != nil {
		_ = fh.Close()
		return fmt.Errorf("logging: stat log file %q: %w", name, err)
	}
	w.file, w.curName, w.curDate, w.curSize = fh, name, today, st.Size()
	w.enforceLocked()
	return nil
}

// Write appends one formatted record (or record fragment). It rolls
// over when the date changed or the current file would exceed
// maxFileBytes.
func (w *categoryWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	today := dayOf(w.now())
	needRoll := !w.curDate.Equal(today) || (w.curSize > 0 && w.curSize+int64(len(p)) > maxFileBytes)
	if needRoll {
		if err := w.rollLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.curSize += int64(n)
	w.writes++
	if w.writes%128 == 0 {
		w.enforceLocked()
	}
	return n, err
}

// enforceLocked applies retention and capacity limits. Failures are
// recorded for diagnostics and never propagated: log cleanup must
// not break logging or startup.
func (w *categoryWriter) enforceLocked() {
	files := w.scanLocked()
	cutoff := dayOf(w.now()).AddDate(0, 0, -(w.policy.RetentionDays - 1))
	var total int64
	var kept []fileEntry
	for _, f := range files {
		if f.name == w.curName {
			kept = append(kept, f)
			total += f.size
			continue
		}
		if f.date.Before(cutoff) {
			w.removeLocked(f)
			continue
		}
		kept = append(kept, f)
		total += f.size
	}
	// Capacity: delete oldest first, never the current file.
	for total > w.policy.CapBytes {
		idx := -1
		for i, f := range kept {
			if f.name != w.curName {
				idx = i
				break
			}
		}
		if idx < 0 {
			break // only the current file is left
		}
		total -= kept[idx].size
		w.removeLocked(kept[idx])
		kept = append(kept[:idx], kept[idx+1:]...)
	}
}

func (w *categoryWriter) removeLocked(f fileEntry) {
	if err := os.Remove(filepath.Join(w.dir, f.name)); err != nil && !os.IsNotExist(err) {
		w.cleanupErr = err
		w.cleanupErrN++
	}
}

// stats snapshots the category's files for diagnostics.
func (w *categoryWriter) stats() (files []fileEntry, cleanupErr error, cleanupErrN int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.scanLocked(), w.cleanupErr, w.cleanupErrN
}

// Close flushes and closes the current file.
func (w *categoryWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func dayOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}
