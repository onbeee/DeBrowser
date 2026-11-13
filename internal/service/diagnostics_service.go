package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/version"
)

// DiagnosticsService (Stage 5B) backs the diagnostics page: a
// read-only view of the application's version, database schema and
// logging-system state, plus the one-click diagnostic export.
//
// The export is deliberately narrow: the ZIP contains ONLY the
// three categorized log streams and two metadata documents
// (manifest.json, acceptance-record.json). It never includes the
// database, the credential vault, browser profiles, settings or any
// other business data, and every file is screened for sensitive
// content before it is added — a single hit aborts the export.
type DiagnosticsService struct {
	db   *database.DB
	logs *logging.System
	app  *AppService

	// SavePathPicker asks the user where to save the export (the
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

// CategoryView is the diagnostics view of one log category.
type CategoryView struct {
	Category      string `json:"category"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	RetentionDays int    `json:"retentionDays"`
	CapBytes      int64  `json:"capBytes"`
	CleanupErrors int    `json:"cleanupErrors"`
}

// ExportSummary describes a completed export.
type ExportSummary struct {
	At    string `json:"at"`
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// DiagnosticsView is everything the diagnostics page shows.
type DiagnosticsView struct {
	AppName        string         `json:"appName"`
	AppVersion     string         `json:"appVersion"`
	SchemaVersion  int            `json:"schemaVersion"`
	DatabaseStatus string         `json:"databaseStatus"`
	UptimeSeconds  int64          `json:"uptimeSeconds"`
	LogDir         string         `json:"logDir"`
	Categories     []CategoryView `json:"categories"`
	LastExport     *ExportSummary `json:"lastExport"`
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
		LogDir:         s.logs.Dir(),
	}
	for _, st := range s.logs.Stats() {
		view.Categories = append(view.Categories, CategoryView{
			Category:      string(st.Category),
			Files:         st.Files,
			Bytes:         st.Bytes,
			RetentionDays: st.Policy.RetentionDays,
			CapBytes:      st.Policy.CapBytes,
			CleanupErrors: st.CleanupErrors,
		})
	}
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

// ExportDiagnostics asks the user for a destination (native save
// dialog) and writes the diagnostic ZIP there. Cancelling the
// dialog is a normal outcome, not an error.
func (s *DiagnosticsService) ExportDiagnostics() (ExportResult, error) {
	start := time.Now()
	if s.SavePathPicker == nil {
		return ExportResult{}, fmt.Errorf("保存对话框不可用")
	}
	defaultName := "debrowser-diagnostics-" + time.Now().Format("20060102-150405") + ".zip"
	dest, err := s.SavePathPicker(defaultName)
	if err != nil {
		auditOp(s.logs, "diagnostics.export", "diagnostics", "", start, err)
		return ExportResult{}, fmt.Errorf("打开保存对话框失败：%w", err)
	}
	if dest == "" {
		return ExportResult{Cancelled: true}, nil
	}
	summary, err := s.exportTo(dest)
	auditOp(s.logs, "diagnostics.export", "diagnostics", "", start, err)
	if err != nil {
		return ExportResult{}, err
	}
	s.mu.Lock()
	s.lastExport = &summary
	s.mu.Unlock()
	return ExportResult{Path: summary.Path, Files: summary.Files, Bytes: summary.Bytes}, nil
}

// exportTo builds the diagnostic ZIP at dest. The archive is first
// written to a temporary file in the same directory and renamed
// into place, so a failure never leaves a partial export behind and
// never touches existing data.
func (s *DiagnosticsService) exportTo(dest string) (ExportSummary, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	schema, err := s.db.AppliedVersion(ctx)
	cancel()
	if err != nil {
		return ExportSummary{}, fmt.Errorf("读取数据库结构版本失败：%w", err)
	}
	status := s.app.GetStatus()
	exportedAt := time.Now()

	type pendingFile struct {
		arcName string
		data    []byte
		sha256  string
	}
	var pending []pendingFile
	var manifestFiles []map[string]any
	var categoryMeta []map[string]any
	totalFiles := 0
	var totalBytes int64

	for _, st := range s.logs.Stats() {
		files := s.logs.Files(st.Category)
		var catBytes int64
		oldest, newest := "", ""
		for _, f := range files {
			raw, err := os.ReadFile(f.Path)
			if err != nil {
				return ExportSummary{}, fmt.Errorf("读取日志文件失败（%s）：%w", f.Name, err)
			}
			// Historical log lines may still contain absolute
			// paths (from before they were removed at the source,
			// or inside error text). Redact them in memory — the
			// file on disk is never modified — then validate the
			// JSON Lines format and run the sensitivity scan on
			// exactly the bytes that will be archived and hashed.
			data := redactPaths(raw)
			if err := validateJSONLines(data); err != nil {
				return ExportSummary{}, fmt.Errorf("日志格式验证未通过（%s）：%w", f.Name, err)
			}
			if err := ScanForSecrets(data); err != nil {
				return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（%s）：%w", f.Name, err)
			}
			sum := sha256.Sum256(data)
			arc := "logs/" + string(st.Category) + "/" + f.Name
			pending = append(pending, pendingFile{arcName: arc, data: data, sha256: hex.EncodeToString(sum[:])})
			manifestFiles = append(manifestFiles, map[string]any{
				"path": arc, "size": len(data), "sha256": hex.EncodeToString(sum[:]),
			})
			catBytes += int64(len(data))
			if oldest == "" {
				oldest = f.Name
			}
			newest = f.Name
		}
		categoryMeta = append(categoryMeta, map[string]any{
			"category": string(st.Category), "files": len(files), "bytes": catBytes,
			"oldestFile": oldest, "newestFile": newest,
			"retentionDays": st.Policy.RetentionDays, "capBytes": st.Policy.CapBytes,
		})
		totalFiles += len(files)
		totalBytes += catBytes
	}

	manifest := map[string]any{
		"formatVersion": 1,
		"appName":       version.Name,
		"appVersion":    version.Version,
		"schemaVersion": schema,
		"exportedAt":    exportedAt.Format(time.RFC3339),
		"categories":    categoryMeta,
		"files":         manifestFiles,
	}
	acceptance := map[string]any{
		"appName":       version.Name,
		"appVersion":    version.Version,
		"schemaVersion": schema,
		"platform":      runtime.GOOS + "/" + runtime.GOARCH,
		"exportedAt":    exportedAt.Format(time.RFC3339),
		"status": map[string]any{
			"database":      status.Database,
			"uptimeSeconds": status.UptimeSeconds,
		},
		"logging": map[string]any{
			"categories": categoryMeta,
			"totalFiles": totalFiles,
			"totalBytes": totalBytes,
		},
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ExportSummary{}, err
	}
	acceptanceJSON, err := json.MarshalIndent(acceptance, "", "  ")
	if err != nil {
		return ExportSummary{}, err
	}
	// Both documents are path-free by construction (relative names
	// only); run them through the same redaction as a guarantee.
	manifestJSON = redactPaths(manifestJSON)
	acceptanceJSON = redactPaths(acceptanceJSON)
	if err := ScanForSecrets(manifestJSON); err != nil {
		return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（manifest.json）：%w", err)
	}
	if err := ScanForSecrets(acceptanceJSON); err != nil {
		return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（acceptance-record.json）：%w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".diagnostics-*.tmp")
	if err != nil {
		return ExportSummary{}, fmt.Errorf("创建导出文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	zw := zip.NewWriter(tmp)
	writeEntry := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := writeEntry("manifest.json", manifestJSON); err != nil {
		_ = tmp.Close()
		return ExportSummary{}, fmt.Errorf("写入导出文件失败：%w", err)
	}
	if err := writeEntry("acceptance-record.json", acceptanceJSON); err != nil {
		_ = tmp.Close()
		return ExportSummary{}, fmt.Errorf("写入导出文件失败：%w", err)
	}
	for _, pf := range pending {
		if err := writeEntry(pf.arcName, pf.data); err != nil {
			_ = tmp.Close()
			return ExportSummary{}, fmt.Errorf("写入导出文件失败：%w", err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return ExportSummary{}, fmt.Errorf("完成导出文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return ExportSummary{}, fmt.Errorf("完成导出文件失败：%w", err)
	}
	// The user picked this destination in the save dialog. Replace
	// it with the platform's atomic replace (MoveFileExW with
	// REPLACE_EXISTING | WRITE_THROUGH on Windows, rename on
	// Unix): the destination is never deleted first, so a failure
	// leaves a previous export byte-identical, and the deferred
	// cleanup above removes the temporary file.
	if err := s.replaceFile(tmpName, dest); err != nil {
		return ExportSummary{}, fmt.Errorf("保存导出文件失败：%w", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return ExportSummary{}, err
	}
	return ExportSummary{
		At:    exportedAt.Format(time.RFC3339),
		Path:  dest,
		Files: totalFiles,
		Bytes: st.Size(),
	}, nil
}

// Sensitivity screening for exported content. The log pipeline is
// safe by construction (whitelist audit records, sanitizing guard),
// so this scan is the second line of defense: if anything that
// looks like a credential still slipped into a log line, the export
// fails closed instead of shipping it.
var (
	secretRefMarker   = []byte("secret_ref")
	vaultEntriesMark  = []byte("\"entries\"")
	urlUserinfoScanRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s/"']+@`)
	jsonSecretKeyRe   = regexp.MustCompile(`(?i)"(password|passwd|pwd|secret|token|secret_ref)"\s*:`)
)

// ScanForSecrets reports whether data contains credential-shaped
// content that must never leave the machine in a diagnostic export.
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
