package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
)

// Category identifies one of the three log streams.
type Category string

const (
	CategoryRun       Category = "run"
	CategoryOperation Category = "operation"
	CategoryError     Category = "error"
)

// Stage 5B retention policy (fixed by the product specification):
//
//	run:       30 days, 20 MB total
//	operation: 90 days, 50 MB total
//	error:     90 days, 50 MB total
var categoryPolicies = map[Category]Policy{
	CategoryRun:       {RetentionDays: 30, CapBytes: 20 << 20},
	CategoryOperation: {RetentionDays: 90, CapBytes: 50 << 20},
	CategoryError:     {RetentionDays: 90, CapBytes: 50 << 20},
}

// Policy returns the retention policy of a category.
func PolicyFor(c Category) Policy { return categoryPolicies[c] }

// System is the Stage 5B logging system: three categorized,
// independently retained JSON Lines streams under data/logs/ plus
// the *slog.Logger the application has always used (now writing to
// the run category through the sanitizing guard handler, with error
// records tee'd into the error category).
type System struct {
	dir     string
	writers map[Category]*categoryWriter
	logger  *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

// NewSystem creates the three category writers under the layout's
// logs directory and builds the application logger. Retention and
// capacity enforcement run during creation; a cleanup failure is
// recorded for diagnostics and never fails startup.
func NewSystem(cfg config.Config) (*System, error) {
	dir := cfg.Layout.Logs
	s := &System{dir: dir, writers: make(map[Category]*categoryWriter)}
	for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
		w, err := newCategoryWriter(dir, string(cat), categoryPolicies[cat])
		if err != nil {
			for _, other := range s.writers {
				_ = other.Close()
			}
			return nil, err
		}
		s.writers[cat] = w
	}
	level := ParseLevel(cfg.LogLevel)
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: renameAttr}
	runHandler := slog.NewJSONHandler(io.MultiWriter(s.writers[CategoryRun], os.Stderr), opts).
		WithAttrs([]slog.Attr{slog.String("category", string(CategoryRun))})
	errHandler := slog.NewJSONHandler(s.writers[CategoryError], opts).
		WithAttrs([]slog.Attr{slog.String("category", string(CategoryError))})
	tee := &teeHandler{run: runHandler, errs: errHandler, level: level}
	s.logger = slog.New(newGuardHandler(tee))
	return s, nil
}

// Logger returns the application logger (run category; errors are
// also copied into the error category). Existing call sites keep
// using *slog.Logger unchanged.
func (s *System) Logger() *slog.Logger { return s.logger }

// Dir returns the logs directory (data/logs).
func (s *System) Dir() string { return s.dir }

// AuditEvent is one operation-audit record. It is a fixed whitelist
// by construction: callers can only supply these fields, so no
// business object, credential or free-form user text can reach the
// operation log through this API.
type AuditEvent struct {
	Operation  string        // e.g. "instance.create"
	TargetType string        // e.g. "instance", "proxy", "settings"
	TargetID   string        // opaque object ID; "" when not applicable
	OK         bool          // operation succeeded
	Duration   time.Duration // wall time of the operation
	ErrClass   string        // safe error classification on failure; "" on success
}

// Audit appends one record to the operation log. It never fails the
// caller: a logging problem must not break the business operation
// it describes.
func (s *System) Audit(e AuditEvent) {
	rec := map[string]any{
		"timestamp":  time.Now().Format(time.RFC3339Nano),
		"level":      "INFO",
		"category":   string(CategoryOperation),
		"event":      "audit",
		"operation":  e.Operation,
		"targetType": e.TargetType,
		"result":     "failure",
		"durationMs": e.Duration.Milliseconds(),
	}
	if e.TargetID != "" {
		rec["targetId"] = e.TargetID
	}
	if e.OK {
		rec["result"] = "success"
	} else if e.ErrClass != "" {
		rec["error"] = e.ErrClass
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = s.writers[CategoryOperation].Write(append(line, '\n'))
}

// CategoryStats is a point-in-time view of one category for the
// diagnostics page and exports.
type CategoryStats struct {
	Category      Category
	Files         int
	Bytes         int64
	OldestFile    string
	NewestFile    string
	Policy        Policy
	CleanupErrors int
}

// Stats returns per-category file counts and sizes.
func (s *System) Stats() []CategoryStats {
	out := make([]CategoryStats, 0, 3)
	for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
		w := s.writers[cat]
		files, _, errN := w.stats()
		st := CategoryStats{Category: cat, Policy: w.policy, CleanupErrors: errN}
		for _, f := range files {
			st.Files++
			st.Bytes += f.size
		}
		if len(files) > 0 {
			st.OldestFile = files[0].name
			st.NewestFile = files[len(files)-1].name
		}
		out = append(out, st)
	}
	return out
}

// LogFile describes one log file for diagnostic export.
type LogFile struct {
	Category Category
	Name     string // base name
	Path     string // absolute path (internal use only; never exported)
	Size     int64
}

// Files lists the current log files of one category, oldest first.
func (s *System) Files(cat Category) []LogFile {
	files, _, _ := s.writers[cat].stats()
	out := make([]LogFile, 0, len(files))
	for _, f := range files {
		out = append(out, LogFile{
			Category: cat,
			Name:     f.name,
			Path:     filepath.Join(s.dir, f.name),
			Size:     f.size,
		})
	}
	return out
}

// Close closes all category writers. Safe to call more than once.
func (s *System) Close() error {
	s.closeOnce.Do(func() {
		for _, cat := range []Category{CategoryRun, CategoryOperation, CategoryError} {
			if err := s.writers[cat].Close(); err != nil && s.closeErr == nil {
				s.closeErr = fmt.Errorf("logging: close %s log: %w", cat, err)
			}
		}
	})
	return s.closeErr
}
