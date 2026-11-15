package logging

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

// Global log capacity (unified recording system). The three
// category streams share ONE user-configurable total capacity —
// there are no per-category byte caps any more (the Stage 5B
// per-category day retentions still apply on top). When the total
// size of the log files exceeds the limit, enforcement removes the
// OLDEST records first:
//
//  1. closed (already rotated) files are deleted whole, oldest
//     first across all categories — a whole file is a run of
//     complete JSON lines, so file deletion can never corrupt the
//     JSONL structure;
//  2. if only the files currently being written remain and the
//     total is still over the limit, the oldest current file is
//     trimmed line-wise: complete operation groups (all lines
//     sharing one operation_id) are dropped oldest-first, and
//     inside the last partially-dropped group the oldest lines go
//     first but the group's final line — its result — is kept.
//     A single remaining line is never split or dropped.
//
// The file currently being written is never deleted. Enforcement
// failures are recorded (see CapacityError) and reported to the
// caller of SetCapacityLimit instead of being hidden.

// DefaultCapacityBytes is the total log capacity used until the
// user configures one: 100 MB.
const DefaultCapacityBytes int64 = 100 << 20

// MinCapacityBytes is the smallest configurable total capacity.
const MinCapacityBytes int64 = 1 << 20

// MaxCapacityBytes is the largest configurable total capacity.
const MaxCapacityBytes int64 = 1024 << 30

// CapacityLimit returns the configured total capacity in bytes.
func (s *System) CapacityLimit() int64 {
	if s == nil {
		return DefaultCapacityBytes
	}
	if v := atomic.LoadInt64(&s.capacityLimit); v > 0 {
		return v
	}
	return DefaultCapacityBytes
}

// TotalBytes returns the actual on-disk size of all log files in
// all categories, measured from the filesystem (never estimated).
func (s *System) TotalBytes() int64 {
	var total int64
	for _, st := range s.Stats() {
		total += st.Bytes
	}
	return total
}

// RemainingBytes returns limit − used, floored at 0.
func (s *System) RemainingBytes() int64 {
	rem := s.CapacityLimit() - s.TotalBytes()
	if rem < 0 {
		return 0
	}
	return rem
}

// CapacityError returns the most recent capacity-enforcement
// failure and how many have occurred, for diagnostics.
func (s *System) CapacityError() (error, int) {
	if s == nil {
		return nil, 0
	}
	s.capMu.Lock()
	defer s.capMu.Unlock()
	return s.capErr, s.capErrN
}

func (s *System) recordCapacityError(err error) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	s.capErr = err
	s.capErrN++
}

// SetCapacityLimit validates and installs a new total capacity and
// enforces it immediately: if the current usage exceeds the new
// limit, the oldest records are cleaned up right away. It returns
// the post-enforcement usage; a non-nil error means enforcement
// could not reach the limit (for example a locked or failing
// file) — the limit itself stays installed and the failure is
// reported, never hidden.
func (s *System) SetCapacityLimit(bytes int64) (int64, error) {
	if bytes < MinCapacityBytes || bytes > MaxCapacityBytes {
		return s.TotalBytes(), fmt.Errorf("日志容量必须在 %d MB 到 %d GB 之间", MinCapacityBytes>>20, MaxCapacityBytes>>30)
	}
	atomic.StoreInt64(&s.capacityLimit, bytes)
	err := s.EnforceCapacity()
	return s.TotalBytes(), err
}

// EnforceCapacity brings the total log size within the configured
// limit, deleting/trimming oldest records first. It is safe for
// concurrent use; concurrent invocations are coalesced.
func (s *System) EnforceCapacity() error {
	if s == nil {
		return nil
	}
	if !s.enforceSem.TryLock() {
		return nil // another enforcement is already running
	}
	defer s.enforceSem.Unlock()
	limit := s.CapacityLimit()
	total := s.TotalBytes()
	if total <= limit {
		return nil
	}

	// Phase 1: delete whole closed files, oldest first across all
	// categories.
	type closedFile struct {
		cat  Category
		name string
		date time.Time
		vol  int
		size int64
	}
	var closed []closedFile
	for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
		w := s.writers[cat]
		files, _, _ := w.stats()
		for _, f := range files {
			if f.name == w.currentName() {
				continue
			}
			closed = append(closed, closedFile{cat: cat, name: f.name, date: f.date, vol: f.vol, size: f.size})
		}
	}
	sort.Slice(closed, func(i, j int) bool {
		if !closed[i].date.Equal(closed[j].date) {
			return closed[i].date.Before(closed[j].date)
		}
		if closed[i].vol != closed[j].vol {
			return closed[i].vol < closed[j].vol
		}
		return closed[i].cat < closed[j].cat
	})
	for _, cf := range closed {
		if total <= limit {
			break
		}
		if s.writers[cf.cat].deleteClosed(cf.name) {
			total -= cf.size
		}
	}

	// Phase 2: trim the oldest current files line-wise.
	for total > limit {
		cat, ok := s.oldestCurrentStream()
		if !ok {
			break
		}
		removed, err := s.writers[cat].trimCurrent(total - limit)
		if err != nil {
			s.recordCapacityError(err)
			return fmt.Errorf("清理最旧日志文件失败：%w", err)
		}
		if removed <= 0 {
			break // nothing more can be removed from any current file
		}
		total -= removed
	}

	total = s.TotalBytes()
	if total > limit {
		err := fmt.Errorf("日志容量清理后仍超出上限：当前 %d 字节，上限 %d 字节", total, limit)
		s.recordCapacityError(err)
		return err
	}
	return nil
}

// oldestCurrentStream picks the category whose current file holds
// the oldest first record, so trimming removes the oldest records
// in the whole store first.
func (s *System) oldestCurrentStream() (Category, bool) {
	var best Category
	var bestTime time.Time
	found := false
	for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
		w := s.writers[cat]
		ts, ok := w.firstRecordTime()
		if !ok {
			continue
		}
		if !found || ts.Before(bestTime) {
			best, bestTime, found = cat, ts, true
		}
	}
	return best, found
}

// enforceThrottled is the writers' write-threshold callback: it
// enforces capacity at most once per second and only when over
// the limit, so steady-state writing pays one directory scan.
func (s *System) enforceThrottled() {
	now := time.Now()
	s.capMu.Lock()
	if now.Sub(s.lastEnforce) < time.Second {
		s.capMu.Unlock()
		return
	}
	s.lastEnforce = now
	s.capMu.Unlock()
	if s.TotalBytes() > s.CapacityLimit() {
		_ = s.EnforceCapacity()
	}
}

// --- categoryWriter additions for capacity enforcement ---

// currentName returns the name of the file currently being written.
func (w *categoryWriter) currentName() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.curName
}

// deleteClosed removes a rotated file of this category. It refuses
// the file currently being written and reports whether a file was
// actually removed.
func (w *categoryWriter) deleteClosed(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || name == w.curName {
		return false
	}
	if err := os.Remove(filepath.Join(w.dir, name)); err != nil {
		if os.IsNotExist(err) {
			return false
		}
		w.cleanupErr = err
		w.cleanupErrN++
		return false
	}
	return true
}

// firstRecordTime reads the timestamp of the first parseable
// record in the current file.
func (w *categoryWriter) firstRecordTime() (time.Time, bool) {
	w.mu.Lock()
	name := w.curName
	w.mu.Unlock()
	if name == "" {
		return time.Time{}, false
	}
	fh, err := os.Open(filepath.Join(w.dir, name))
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		if ts, ok := parseRecordTimestamp(sc.Bytes()); ok {
			return ts, true
		}
	}
	return time.Time{}, false
}

// trimCurrent removes at least removeBytes worth of the oldest
// records from the file currently being written, honouring the
// operation-group rule documented at the top of this file. The
// rewrite is atomic (temporary file + rename) and the writer
// reopens the file for appending; concurrent Write calls block on
// the writer mutex for the duration. It returns the number of
// bytes actually removed (0 when nothing can be removed).
func (w *categoryWriter) trimCurrent(removeBytes int64) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil || removeBytes <= 0 {
		return 0, nil
	}
	path := filepath.Join(w.dir, w.curName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("logging: read current log for trimming: %w", err)
	}
	kept, removed := trimOldestRecords(raw, removeBytes)
	if removed <= 0 {
		return 0, nil
	}
	if err := w.file.Close(); err != nil {
		return 0, fmt.Errorf("logging: close log for trimming: %w", err)
	}
	w.file = nil
	tmp, err := os.CreateTemp(w.dir, ".trim-*.tmp")
	if err != nil {
		w.reopenAfterTrimFailed(path)
		return 0, fmt.Errorf("logging: create trimming temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(kept); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		w.reopenAfterTrimFailed(path)
		return 0, fmt.Errorf("logging: write trimmed log: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		w.reopenAfterTrimFailed(path)
		return 0, fmt.Errorf("logging: finish trimmed log: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		w.reopenAfterTrimFailed(path)
		return 0, fmt.Errorf("logging: replace trimmed log: %w", err)
	}
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return removed, fmt.Errorf("logging: reopen trimmed log: %w", err)
	}
	w.file = fh
	w.curSize = int64(len(kept))
	return removed, nil
}

// reopenAfterTrimFailed makes a best effort to restore the append
// handle after a failed trim so logging can continue.
func (w *categoryWriter) reopenAfterTrimFailed(path string) {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		w.cleanupErr = err
		w.cleanupErrN++
		return
	}
	w.file = fh
	if st, err := fh.Stat(); err == nil {
		w.curSize = st.Size()
	}
}

// trimOldestRecords drops the oldest records from raw JSONL data
// until at least removeBytes have been removed (or nothing more
// can be dropped). Records sharing an operation_id form a group
// and are dropped together, oldest group first; when a group must
// be partially dropped, its oldest lines go first but its last
// line (the result) is kept. The file's newest record (its last
// complete line) is never dropped: if it alone exceeds the
// capacity, enforcement reports that the limit cannot be reached
// instead of destroying the most recent record. Lines without an
// operation_id are singleton groups. A trailing partial line
// (torn write) is preserved byte-for-byte at the end.
func trimOldestRecords(raw []byte, removeBytes int64) (kept []byte, removed int64) {
	if removeBytes <= 0 || len(raw) == 0 {
		return raw, 0
	}
	torn := []byte(nil)
	if !bytes.HasSuffix(raw, []byte("\n")) {
		if idx := bytes.LastIndexByte(raw, '\n'); idx >= 0 {
			torn = raw[idx+1:]
			raw = raw[:idx+1]
		} else {
			return raw, 0 // one partial line only; never touch it
		}
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1] // SplitAfter leaves a trailing empty element
	}
	if len(lines) == 0 {
		return raw, 0
	}
	// Group consecutive lines by operation_id (an operation's
	// records are consecutive within one stream in practice; a
	// repeated ID later starts a new group, which only makes the
	// grouping more conservative).
	type group struct{ start, end int } // line indexes [start, end)
	var groups []group
	lineOp := make([]string, len(lines))
	for i, ln := range lines {
		lineOp[i] = recordOperationID(ln)
	}
	for i := 0; i < len(lines); {
		j := i + 1
		if lineOp[i] != "" {
			for j < len(lines) && lineOp[j] == lineOp[i] {
				j++
			}
		}
		groups = append(groups, group{start: i, end: j})
		i = j
	}
	drop := make([]bool, len(lines))
	lastLine := len(lines) - 1
	for _, g := range groups {
		if removed >= removeBytes {
			break
		}
		// The group holding the file's newest record may only be
		// partially dropped: its final line always stays.
		holdsNewest := g.end-1 == lastLine
		var gsize int64
		for i := g.start; i < g.end; i++ {
			gsize += int64(len(lines[i]))
		}
		if !holdsNewest && (removed+gsize <= removeBytes || g.end-g.start == 1) {
			for i := g.start; i < g.end; i++ {
				drop[i] = true
			}
			removed += gsize
			continue
		}
		// Partial group: drop oldest lines but keep the last one.
		for i := g.start; i < g.end-1 && removed < removeBytes; i++ {
			drop[i] = true
			removed += int64(len(lines[i]))
		}
	}
	if removed == 0 {
		return append(raw, torn...), 0
	}
	var out bytes.Buffer
	for i, ln := range lines {
		if !drop[i] {
			out.Write(ln)
		}
	}
	out.Write(torn)
	return out.Bytes(), removed
}

// recordOperationID extracts the operation_id of a unified event
// line, or "" for legacy lines.
func recordOperationID(line []byte) string {
	var probe struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return ""
	}
	return probe.OperationID
}

// parseRecordTimestamp extracts a record's timestamp from either
// the unified "timestamp" field or the legacy slog "time" field.
func parseRecordTimestamp(line []byte) (time.Time, bool) {
	var probe struct {
		Timestamp string `json:"timestamp"`
		Time      string `json:"time"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return time.Time{}, false
	}
	for _, v := range []string{probe.Timestamp, probe.Time} {
		if v == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, v); err == nil {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}
