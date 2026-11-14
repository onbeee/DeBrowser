package service

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/store"
)

type diagFixture struct {
	svc    *DiagnosticsService
	layout paths.Layout
	logs   *logging.System
	db     *database.DB
}

func newDiagFixture(t *testing.T) diagFixture {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Layout: layout, LogLevel: "info"}
	db, err := database.Open(layout.DatabaseFile)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	logs, err := logging.NewSystem(cfg)
	if err != nil {
		t.Fatalf("NewSystem: %v", err)
	}
	appSvc, err := NewAppService(cfg, db, logs.Logger())
	if err != nil {
		t.Fatalf("NewAppService: %v", err)
	}
	f := diagFixture{svc: NewDiagnosticsService(db, logs, appSvc), layout: layout, logs: logs, db: db}
	t.Cleanup(func() {
		_ = logs.Close()
		_ = db.Close()
	})
	return f
}

func TestGetDiagnostics(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.Logger().Info("hello")
	view, err := f.svc.GetDiagnostics()
	if err != nil {
		t.Fatalf("GetDiagnostics: %v", err)
	}
	if view.AppVersion == "" || view.SchemaVersion != 9 {
		t.Fatalf("view = %+v, want version set and schema 5", view)
	}
	if len(view.Categories) != 3 {
		t.Fatalf("categories = %d, want 3", len(view.Categories))
	}
	if view.LastExport != nil {
		t.Fatalf("LastExport = %+v, want nil before any export", view.LastExport)
	}
}

func TestExportDiagnosticsZipStructureAndHashes(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.Logger().Info("run event")
	f.logs.Audit(logging.AuditEvent{Operation: "proxy.create", TargetType: "proxy", TargetID: "p1", OK: true})

	dest := filepath.Join(t.TempDir(), "diag.zip")
	summary, err := f.svc.exportTo(dest, nil)
	if err != nil {
		t.Fatalf("exportTo: %v", err)
	}
	if summary.Files < 3 || summary.Bytes <= 0 {
		t.Fatalf("summary = %+v", summary)
	}
	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	names := map[string]*zip.File{}
	for _, zf := range zr.File {
		names[zf.Name] = zf
		if strings.Contains(zf.Name, "debrowser.db") || strings.Contains(zf.Name, "secrets.vault") ||
			strings.Contains(zf.Name, "profiles") || strings.Contains(zf.Name, "settings.json") {
			t.Fatalf("export contains forbidden entry %q", zf.Name)
		}
	}
	for _, want := range []string{"manifest.json", "acceptance-record.json"} {
		if names[want] == nil {
			t.Fatalf("zip missing %s (has %v)", want, keys(names))
		}
	}
	// Manifest hashes must match the archived file bytes.
	mf := readZipEntry(t, names["manifest.json"])
	var manifest struct {
		FormatVersion int `json:"formatVersion"`
		SchemaVersion int `json:"schemaVersion"`
		Files         []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(mf, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest.FormatVersion != 1 || manifest.SchemaVersion != 9 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("manifest lists no log files")
	}
	for _, entry := range manifest.Files {
		zf := names[entry.Path]
		if zf == nil {
			t.Fatalf("manifest file %s not in zip", entry.Path)
		}
		sum := sha256.Sum256(readZipEntry(t, zf))
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Fatalf("sha256 mismatch for %s", entry.Path)
		}
	}
	// The manifest must not leak absolute machine paths.
	if strings.Contains(string(mf), f.layout.Root) {
		t.Fatalf("manifest contains an absolute path: %s", mf)
	}
	// Acceptance record carries status + policy.
	ar := readZipEntry(t, names["acceptance-record.json"])
	for _, want := range []string{"appVersion", "schemaVersion", "platform", "logging"} {
		if !strings.Contains(string(ar), want) {
			t.Fatalf("acceptance record missing %q: %s", want, ar)
		}
	}
}

func TestExportFailsClosedOnPoisonedLog(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.Logger().Info("clean line")
	_ = f.logs.Close()
	// Simulate a hypothetical secret leak in a log file: the export
	// must refuse rather than ship it.
	runFile := ""
	entries, _ := os.ReadDir(f.layout.Logs)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "run-") {
			runFile = filepath.Join(f.layout.Logs, e.Name())
		}
	}
	fh, err := os.OpenFile(runFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString("{\"leak\":\"secret_ref = abc\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()

	dest := filepath.Join(t.TempDir(), "diag.zip")
	if _, err := f.svc.exportTo(dest, nil); err == nil {
		t.Fatal("exportTo succeeded despite a poisoned log file")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("a partial/forbidden export exists at %s", dest)
	}
	// Existing data is untouched.
	if _, err := os.Stat(f.layout.DatabaseFile); err != nil {
		t.Fatalf("database file damaged by failed export: %v", err)
	}
	if _, err := os.Stat(runFile); err != nil {
		t.Fatalf("log file damaged by failed export: %v", err)
	}
}

func TestExportUnwritableDestination(t *testing.T) {
	f := newDiagFixture(t)
	dest := filepath.Join(t.TempDir(), "missing-dir", "diag.zip")
	if _, err := f.svc.exportTo(dest, nil); err == nil {
		t.Fatal("exportTo succeeded with an unwritable destination")
	}
}

func TestExportCancelledDialog(t *testing.T) {
	f := newDiagFixture(t)
	f.svc.SavePathPicker = func(string) (string, error) { return "", nil }
	res, err := f.svc.ExportDiagnostics()
	if err != nil {
		t.Fatalf("ExportDiagnostics: %v", err)
	}
	if !res.Cancelled {
		t.Fatalf("result = %+v, want Cancelled", res)
	}
}

func TestScanForSecrets(t *testing.T) {
	bad := []string{
		`{"secret_ref":"x"}`,
		`{"entries":{}}`,
		"dial https://user:pw@example.com/x",
		`{"password": "hunter2"}`,
	}
	for _, b := range bad {
		if err := ScanForSecrets([]byte(b)); err == nil {
			t.Fatalf("ScanForSecrets(%q) = nil, want error", b)
		}
	}
	good := []string{
		`{"timestamp":"2026-11-13T00:00:00Z","level":"INFO","category":"run","event":"app started"}`,
		`{"operation":"proxy.create","result":"success"}`,
	}
	for _, g := range good {
		if err := ScanForSecrets([]byte(g)); err != nil {
			t.Fatalf("ScanForSecrets(%q) = %v, want nil", g, err)
		}
	}
}

func keys(m map[string]*zip.File) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func readZipEntry(t *testing.T, zf *zip.File) []byte {
	t.Helper()
	rc, err := zf.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ---- Stage 5B review fixes: path redaction + atomic replace ----

func appendToCategoryFile(t *testing.T, logsDir, prefix, line string) string {
	t.Helper()
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix+"-") {
			p := filepath.Join(logsDir, e.Name())
			fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fh.WriteString(line + "\n"); err != nil {
				t.Fatal(err)
			}
			if err := fh.Close(); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatalf("no %s-* file in %s", prefix, logsDir)
	return ""
}

func hashDirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

func TestExportRedactsAbsolutePaths(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.Logger().Info("normal run line")
	_ = f.logs.Close()

	// Historical lines as they exist on disk (JSON-escaped forms),
	// with absolute paths in field values and in error text.
	appendToCategoryFile(t, f.layout.Logs, "run",
		`{"timestamp":"2026-11-12T08:00:00Z","level":"INFO","category":"run","event":"startup paths","root":"C:\\Users\\Alice\\AppData\\Local\\DeBrowser\\data","other":"D:\\Private\\DeBrowser","share":"\\\\server\\share\\DeBrowser"}`)
	appendToCategoryFile(t, f.layout.Logs, "error",
		`{"timestamp":"2026-11-12T08:01:00Z","level":"ERROR","category":"error","event":"open failed","error":"open /home/alice/.config/debrowser: permission denied; also /Users/alice/Library/Application Support/DeBrowser and /tmp/debrowser-private were tried, see https://example.com/docs for help"}`)

	before := hashDirFiles(t, f.layout.Logs)
	dest := filepath.Join(t.TempDir(), "diag.zip")
	if _, err := f.svc.exportTo(dest, nil); err != nil {
		t.Fatalf("exportTo: %v", err)
	}
	// The original log files must be byte-identical after export.
	after := hashDirFiles(t, f.layout.Logs)
	if len(before) != len(after) {
		t.Fatalf("log file set changed by export: %v -> %v", keys2(before), keys2(after))
	}
	for name, h := range before {
		if after[name] != h {
			t.Fatalf("original log file %s was modified by export", name)
		}
	}

	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	byName := map[string][]byte{}
	for _, zf := range zr.File {
		byName[zf.Name] = readZipEntry(t, zf)
	}
	forbidden := []string{"Alice", "alice",
		`C:\Users`, `C:\\Users`, `D:\Private`, `D:\\Private`,
		`server\share`, `server\\share`,
		"/home/", "/Users/", "Application Support", "/tmp/debrowser-private", ".config/debrowser"}
	manifestRaw, ok := byName["manifest.json"]
	if !ok {
		t.Fatal("zip missing manifest.json")
	}
	acceptRaw, ok := byName["acceptance-record.json"]
	if !ok {
		t.Fatal("zip missing acceptance-record.json")
	}
	for _, doc := range [][]byte{manifestRaw, acceptRaw} {
		for _, bad := range forbidden {
			if strings.Contains(string(doc), bad) {
				t.Fatalf("metadata document contains forbidden %q", bad)
			}
		}
	}
	var manifest struct {
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
			Size   int    `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	sawRedaction := false
	for _, entry := range manifest.Files {
		content, ok := byName[entry.Path]
		if !ok {
			t.Fatalf("manifest lists %s but zip lacks it", entry.Path)
		}
		// Manifest hash and size describe the shipped (redacted) bytes.
		sum := sha256.Sum256(content)
		if hex.EncodeToString(sum[:]) != entry.SHA256 {
			t.Fatalf("manifest sha256 mismatch for %s", entry.Path)
		}
		if entry.Size != len(content) {
			t.Fatalf("manifest size mismatch for %s", entry.Path)
		}
		for _, bad := range forbidden {
			if strings.Contains(string(content), bad) {
				t.Fatalf("exported %s contains forbidden %q:\n%s", entry.Path, bad, content)
			}
		}
		if strings.Contains(string(content), "[REDACTED_PATH]") {
			sawRedaction = true
		}
		// Every exported log line is still valid JSON.
		for i, line := range strings.Split(string(content), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if !json.Valid([]byte(line)) {
				t.Fatalf("%s line %d not valid JSON after redaction: %q", entry.Path, i+1, line)
			}
		}
	}
	if !sawRedaction {
		t.Fatal("no [REDACTED_PATH] placeholder found in exported logs; redaction did not run")
	}
	// Diagnostic value survives: event names and the non-path
	// parts of the error message are preserved, URLs unharmed.
	var errContent string
	for name, content := range byName {
		if strings.HasPrefix(name, "logs/error/") {
			errContent = string(content)
		}
		if strings.HasPrefix(name, "logs/run/") && !strings.Contains(string(content), `"event":"startup paths"`) {
			t.Fatalf("run export lost the event field: %s", content)
		}
	}
	for _, want := range []string{"permission denied", "https://example.com/docs"} {
		if !strings.Contains(errContent, want) {
			t.Fatalf("error export lost %q: %s", want, errContent)
		}
	}
}

func keys2(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExportReplacesExistingDestination(t *testing.T) {
	f := newDiagFixture(t)
	f.logs.Logger().Info("first")
	dest := filepath.Join(t.TempDir(), "diag.zip")
	if _, err := f.svc.exportTo(dest, nil); err != nil {
		t.Fatalf("first exportTo: %v", err)
	}
	f.logs.Logger().Info("second")
	// Second export to the SAME path exercises the platform
	// replace (MoveFileExW REPLACE_EXISTING on Windows CI).
	summary, err := f.svc.exportTo(dest, nil)
	if err != nil {
		t.Fatalf("second exportTo (replace): %v", err)
	}
	if summary.Bytes <= 0 {
		t.Fatalf("summary = %+v", summary)
	}
	zr, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatalf("replaced zip does not open: %v", err)
	}
	defer func() { _ = zr.Close() }()
	found := false
	for _, zf := range zr.File {
		if zf.Name == "manifest.json" {
			found = true
		}
	}
	if !found {
		t.Fatal("replaced zip has no manifest.json")
	}
}

func TestExportReplaceFailureKeepsExistingDest(t *testing.T) {
	f := newDiagFixture(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "diag.zip")
	sentinel := []byte("PREVIOUS EXPORT - DO NOT LOSE")
	if err := os.WriteFile(dest, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.replaceFile = func(string, string) error {
		return errors.New("injected replace failure")
	}
	if _, err := f.svc.exportTo(dest, nil); err == nil {
		t.Fatal("exportTo succeeded despite injected replace failure")
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(sentinel) {
		t.Fatalf("destination changed after failed replace: %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestExportReplaceFailureWithoutDest(t *testing.T) {
	f := newDiagFixture(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "diag.zip")
	f.svc.replaceFile = func(string, string) error {
		return errors.New("injected replace failure")
	}
	if _, err := f.svc.exportTo(dest, nil); err == nil {
		t.Fatal("exportTo succeeded despite injected replace failure")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed export left a target file at %s", dest)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// ---- Fault report (§10 of the Stage 6C redo) ----

type fakeCoreInfo struct {
	logLines []string
	logErr   error
}

func (f fakeCoreInfo) CoreFaultInfo() CoreFaultInfo {
	return CoreFaultInfo{Installed: true, Version: "v1.19.34", Running: false, LastExit: "已退出（退出码 0）", UpdateStatus: "up-to-date", LatestVersion: "v1.19.34"}
}
func (f fakeCoreInfo) GetCoreLog(int) ([]string, error) { return f.logLines, f.logErr }

func zipNames(t *testing.T, path string) map[string]string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	out := map[string]string{}
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		out[f.Name] = string(data)
	}
	return out
}

func TestFaultReportContentsAndRedaction(t *testing.T) {
	f := newDiagFixture(t)
	// Seed a subscription row whose sanitized state should be
	// reported (URL hint only — never the full URL).
	st := store.New(f.db)
	if _, err := st.CreateSubscription(context.Background(), model.Subscription{
		Name: "机场订阅", URLHint: "https://subs.example.com", SecretRef: "diag-ref",
		LastStatus: "error", LastError: "订阅域名解析到不被允许的地址：198.18.1.34 属于基准测试地址，已拒绝连接（subs.example.com）",
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.CoreInfo = fakeCoreInfo{logLines: []string{"time=now level=info msg=started", "time=now level=info msg=ready"}}
	dest := filepath.Join(t.TempDir(), "fault.zip")
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	res, err := f.svc.ExportFaultReport()
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != dest || res.Files == 0 {
		t.Fatalf("result = %+v", res)
	}
	files := zipNames(t, dest)
	report, ok := files["fault-report.json"]
	if !ok {
		t.Fatalf("fault-report.json missing; entries: %v", keysOf(files))
	}
	for _, want := range []string{"\"schemaVersion\": 9", "\"appVersion\"", "v1.19.34", "subs.example.com", "\"mihomoCore\""} {
		if !strings.Contains(report, want) {
			t.Errorf("fault report missing %q:\n%s", want, report)
		}
	}
	if _, ok := files["mihomo-core.log"]; !ok {
		t.Error("mihomo-core.log missing from fault report")
	}
	if _, ok := files["manifest.json"]; !ok {
		t.Error("manifest.json missing")
	}
	// Nothing sensitive or bulky may ride along.
	for name, content := range files {
		if strings.Contains(name, ".db") || strings.Contains(name, "vault") || strings.Contains(name, "profiles/") {
			t.Errorf("forbidden entry in fault report: %s", name)
		}
		if strings.Contains(content, "token=") || strings.Contains(content, "secret-token") {
			t.Errorf("entry %s leaks a URL query/secret", name)
		}
	}
}

func TestFaultReportFailsClosedOnSecretInCoreLog(t *testing.T) {
	f := newDiagFixture(t)
	f.svc.CoreInfo = fakeCoreInfo{logLines: []string{`msg="fetch https://user:pass123@subs.example.com/feed failed"`}}
	dest := filepath.Join(t.TempDir(), "fault.zip")
	f.svc.SavePathPicker = func(string) (string, error) { return dest, nil }
	if _, err := f.svc.ExportFaultReport(); err == nil {
		t.Fatal("fault report exported despite a secret in the core log")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a partial fault report file was left behind")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
