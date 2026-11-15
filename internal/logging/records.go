package logging

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Record scanning for export date ranges and exports. Records are
// read from the stream files on disk; timestamps come from each
// line itself (unified "timestamp" or legacy slog "time"), so the
// reported ranges always reflect the records actually retained —
// they move forward as capacity cleanup removes old records.

// RecordRef is the export-relevant summary of one log line.
type RecordRef struct {
	Timestamp   time.Time
	Category    string // unified category name (operation/error/runtime)
	OperationID string // "" for legacy lines and non-operation records
	EventType   string // "" for legacy lines
}

// scanStream reads every line of one stream, oldest file first.
// damaged counts lines that are not parseable JSON objects (torn
// or corrupt lines): they are skipped, never silently treated as
// records.
func (s *System) scanStream(cat Category, fn func(line []byte, ref RecordRef) bool) (damaged int) {
	files, _, _ := s.writers[cat].stats()
	for _, f := range files {
		fh, err := os.Open(filepath.Join(s.dir, f.name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bufio.NewReaderSize(fh, 64<<10))
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			ts, ok := parseRecordTimestamp(line)
			if !ok {
				damaged++
				continue
			}
			ref := RecordRef{Timestamp: ts, Category: StreamCategoryName(cat)}
			var probe struct {
				OperationID string `json:"operation_id"`
				EventType   string `json:"event_type"`
			}
			if err := json.Unmarshal(line, &probe); err == nil {
				ref.OperationID = probe.OperationID
				ref.EventType = probe.EventType
			}
			if !fn(line, ref) {
				_ = fh.Close()
				return damaged
			}
		}
		_ = fh.Close()
	}
	return damaged
}

// DateRange returns the earliest and latest record timestamps and
// the record count of one unified category ("operation", "error"
// or "runtime"), computed from the records actually on disk.
// ok=false means the category currently holds no records.
func (s *System) DateRange(category string) (earliest, latest time.Time, count int, damaged int, ok bool) {
	stream, valid := streamFor(category)
	if !valid || s == nil {
		return time.Time{}, time.Time{}, 0, 0, false
	}
	damaged = s.scanStream(stream, func(_ []byte, ref RecordRef) bool {
		count++
		if !ok || ref.Timestamp.Before(earliest) {
			earliest = ref.Timestamp
		}
		if !ok || ref.Timestamp.After(latest) {
			latest = ref.Timestamp
		}
		ok = true
		return true
	})
	return earliest, latest, count, damaged, ok
}

// CollectLines returns the raw lines of one unified category whose
// record timestamps fall inside [from, to] (inclusive; a zero bound
// is open), oldest first, together with their refs and the number
// of damaged lines skipped.
func (s *System) CollectLines(category string, from, to time.Time) (lines [][]byte, refs []RecordRef, damaged int) {
	stream, valid := streamFor(category)
	if !valid || s == nil {
		return nil, nil, 0
	}
	damaged = s.scanStream(stream, func(line []byte, ref RecordRef) bool {
		if !from.IsZero() && ref.Timestamp.Before(from) {
			return true
		}
		if !to.IsZero() && ref.Timestamp.After(to) {
			return true
		}
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
		refs = append(refs, ref)
		return true
	})
	return lines, refs, damaged
}
