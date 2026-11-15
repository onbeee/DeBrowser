package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/store"
	"github.com/onbeee/DeBrowser/internal/version"
)

// DiagnosticsService backs the data-management page: a read-only
// view of the application's version, database state and the
// unified logging system (categories, real file names, total
// capacity), plus the per-category record exports implemented in
// logs_export.go.
//
// The former one-click diagnostics export and fault-report
// export were removed by product decision (2026-11-15): the
// unified log records — with their own per-category exports —
// are the single record system, and there is no second audit or
// diagnostics channel. The sensitivity screening below remains
// as the fail-closed guard for the record exports.
type DiagnosticsService struct {
	db   *database.DB
	logs *logging.System
	app  *AppService

	// SavePathPicker asks the user where to save an export (the
	// native Wails save-file dialog, wired by the app assembly in
	// internal/app so this package stays GUI-free). An empty
	// result means "cancelled".
	SavePathPicker func(defaultName string) (string, error)

	// replaceFile atomically replaces the export destination with
	// the finished temporary archive (platform implementation in
	// replace_windows.go / replace_unix.go). It is a field so
	// tests can inject failures.
	replaceFile func(src, dst string) error

	mu         sync.Mutex
	lastExport *ExportSummary
}

// NewDiagnosticsService builds the service.
func NewDiagnosticsService(db *database.DB, logs *logging.System, app *AppService) *DiagnosticsService {
	return &DiagnosticsService{db: db, logs: logs, app: app, replaceFile: platformReplaceFile}
}

// CategoryView is the view of one log category, including the
// real names of the files currently on disk for it (oldest
// first), read live from the log directory — never assumed.
type CategoryView struct {
	Category      string   `json:"category"`
	Files         int      `json:"files"`
	FileNames     []string `json:"fileNames"`
	Bytes         int64    `json:"bytes"`
	RetentionDays int      `json:"retentionDays"`
	CleanupErrors int      `json:"cleanupErrors"`
}

// ExportSummary describes a completed export.
type ExportSummary struct {
	At    string `json:"at"`
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// DiagnosticsView is everything the data-management page shows:
// application and database state, the data directory, instance
// counts and the logging-system categories (run / operation /
// error) with the unified capacity figures.
type DiagnosticsView struct {
	AppName        string         `json:"appName"`
	AppVersion     string         `json:"appVersion"`
	SchemaVersion  int            `json:"schemaVersion"`
	DatabaseStatus string         `json:"databaseStatus"`
	UptimeSeconds  int64          `json:"uptimeSeconds"`
	DataDir        string         `json:"dataDir"`
	DatabaseBytes  int64          `json:"databaseBytes"`
	InstanceCount  int            `json:"instanceCount"`
	LogDir         string         `json:"logDir"`
	Categories     []CategoryView `json:"categories"`
	LastExport     *ExportSummary `json:"lastExport"`
	// Unified log capacity (one total limit across all streams):
	// the configured limit, the actual on-disk usage and the
	// remaining space, all measured from the real files.
	LogCapacityLimitBytes int64 `json:"logCapacityLimitBytes"`
	LogUsedBytes          int64 `json:"logUsedBytes"`
	LogRemainingBytes     int64 `json:"logRemainingBytes"`
	LogWriteErrors        int64 `json:"logWriteErrors"`
}

// GetDiagnostics returns the current diagnostics snapshot.
func (s *DiagnosticsService) GetDiagnostics() (DiagnosticsView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schema, err := s.db.AppliedVersion(ctx)
	if err != nil {
		return DiagnosticsView{}, fmt.Errorf("读取数据库结构版本失败：%w", err)
	}
	status := s.app.GetStatus()
	view := DiagnosticsView{
		AppName:        version.Name,
		AppVersion:     version.Version,
		SchemaVersion:  schema,
		DatabaseStatus: status.Database,
		UptimeSeconds:  status.UptimeSeconds,
		DataDir:        s.app.cfg.Layout.Data,
		LogDir:         s.logs.Dir(),
	}
	// Data overview (data management): the database file size
	// (main file plus its WAL sidecar) and the instance count are
	// read directly, never estimated.
	for _, suffix := range []string{"", "-wal"} {
		if fi, err := os.Stat(s.app.cfg.Layout.DatabaseFile + suffix); err == nil {
			view.DatabaseBytes += fi.Size()
		}
	}
	if instances, err := store.New(s.db).ListInstances(ctx); err == nil {
		view.InstanceCount = len(instances)
	}
	for _, st := range s.logs.Stats() {
		cv := CategoryView{
			Category:      string(st.Category),
			Files:         st.Files,
			Bytes:         st.Bytes,
			RetentionDays: st.Policy.RetentionDays,
			CleanupErrors: st.CleanupErrors,
		}
		for _, f := range s.logs.Files(st.Category) {
			cv.FileNames = append(cv.FileNames, f.Name)
		}
		view.Categories = append(view.Categories, cv)
	}
	view.LogCapacityLimitBytes = s.logs.CapacityLimit()
	view.LogUsedBytes = s.logs.TotalBytes()
	view.LogRemainingBytes = s.logs.RemainingBytes()
	view.LogWriteErrors = s.logs.WriteErrors()
	s.mu.Lock()
	view.LastExport = s.lastExport
	s.mu.Unlock()
	return view, nil
}

// ExportResult reports the outcome of an export request.
type ExportResult struct {
	Cancelled bool   `json:"cancelled"`
	Path      string `json:"path"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
}

// Sensitivity screening for exported content. The log pipeline is
// safe by construction (sanitized events, scrubbed free text), so
// this scan is the second line of defense: if anything that
// looks like a credential still slipped into a log line, the
// export fails closed instead of shipping it.
var (
	secretRefMarker   = []byte("secret_ref")
	vaultEntriesMark  = []byte("\"entries\"")
	urlUserinfoScanRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s/"']+@`)
	jsonSecretKeyRe   = regexp.MustCompile(`(?i)"(password|passwd|pwd|secret|token|secret_ref)"\s*:`)
)

// ScanForSecrets reports whether data contains credential-shaped
// content that must never leave the machine in an export.
func ScanForSecrets(data []byte) error {
	switch {
	case bytes.Contains(data, secretRefMarker):
		return fmt.Errorf("内容包含 secret_ref 标记")
	case bytes.Contains(data, vaultEntriesMark):
		return fmt.Errorf("内容包含 Vault 数据结构")
	case urlUserinfoScanRe.Match(data):
		return fmt.Errorf("内容包含带凭据的 URL")
	case jsonSecretKeyRe.Match(data):
		return fmt.Errorf("内容包含凭据字段")
	default:
		return nil
	}
}
