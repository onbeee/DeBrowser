package service

import (
	"context"
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
	if view.AppVersion == "" || view.SchemaVersion != 12 {
		t.Fatalf("view = %+v, want version set and schema 11", view)
	}
	if len(view.Categories) != 3 {
		t.Fatalf("categories = %d, want 3", len(view.Categories))
	}
	// Every category reports the real names of its files on disk:
	// the count matches and every name carries its .jsonl suffix.
	for _, c := range view.Categories {
		if len(c.FileNames) != c.Files {
			t.Fatalf("category %s: %d file names for %d files", c.Category, len(c.FileNames), c.Files)
		}
		for _, n := range c.FileNames {
			if !strings.HasSuffix(n, ".log") {
				t.Fatalf("category %s: file name %q lacks its .log extension", c.Category, n)
			}
		}
	}
	if view.LastExport != nil {
		t.Fatalf("LastExport = %+v, want nil before any export", view.LastExport)
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
