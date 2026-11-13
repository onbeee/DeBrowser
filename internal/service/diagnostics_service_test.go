package service

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/paths"
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
	if view.AppVersion == "" || view.SchemaVersion != 5 {
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
	summary, err := f.svc.exportTo(dest)
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
	if manifest.FormatVersion != 1 || manifest.SchemaVersion != 5 {
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
	if _, err := f.svc.exportTo(dest); err == nil {
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
	if _, err := f.svc.exportTo(dest); err == nil {
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
