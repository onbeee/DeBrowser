package service

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/version"
)

func TestGetStatusReportsRealState(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{DataDir: dir, DatabasePath: filepath.Join(dir, "test.db")}

	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewAppService(cfg, db, log)

	status := svc.GetStatus()
	if status.Name != version.Name {
		t.Errorf("Name = %q, want %q", status.Name, version.Name)
	}
	if status.Version != version.Version {
		t.Errorf("Version = %q, want %q", status.Version, version.Version)
	}
	if status.DataDir != dir {
		t.Errorf("DataDir = %q, want %q", status.DataDir, dir)
	}
	if status.Database != "ok" {
		t.Errorf("Database = %q, want %q", status.Database, "ok")
	}
	if status.Platform == "" || status.Arch == "" {
		t.Errorf("Platform/Arch must be set, got %q/%q", status.Platform, status.Arch)
	}
	if _, err := time.Parse(time.RFC3339, status.CheckedAtRFC333); err != nil {
		t.Errorf("CheckedAtRFC333 %q is not RFC3339: %v", status.CheckedAtRFC333, err)
	}

	if err := svc.ServiceShutdown(); err != nil {
		t.Errorf("ServiceShutdown() error = %v", err)
	}
}
