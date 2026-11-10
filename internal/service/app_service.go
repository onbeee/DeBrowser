// Package service contains the Wails services exposed to the frontend.
// Services are plain Go structs; their exported methods are callable from
// the frontend through the generated bindings (see frontend/bindings).
package service

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/version"
)

// StatusEvent is the name of the Wails event emitted with a fresh Status
// while the application runs.
const StatusEvent = "app:status"

// Status is a snapshot of the running application's real state, shown in
// the frontend status bar and overview page. It contains no business data.
type Status struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	Platform        string `json:"platform"`
	Arch            string `json:"arch"`
	DataDir         string `json:"dataDir"`
	Database        string `json:"database"` // "ok" or a short error description
	UptimeSeconds   int64  `json:"uptimeSeconds"`
	CheckedAtRFC333 string `json:"checkedAt"`
}

// AppService exposes application-level information and owns the database
// handle for the lifetime of the application.
type AppService struct {
	cfg     config.Config
	db      *database.DB
	log     *slog.Logger
	started time.Time
}

// NewAppService creates the service. The database is migrated by main
// before the service is registered.
func NewAppService(cfg config.Config, db *database.DB, log *slog.Logger) *AppService {
	return &AppService{cfg: cfg, db: db, log: log, started: time.Now()}
}

// GetStatus returns the current application status, including a live
// database health check.
func (s *AppService) GetStatus() Status {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	databaseState := "ok"
	if err := s.db.Health(ctx); err != nil {
		databaseState = "error"
		s.log.Error("database health check failed", "error", err)
	}
	return Status{
		Name:            version.Name,
		Version:         version.Version,
		Platform:        runtime.GOOS,
		Arch:            runtime.GOARCH,
		DataDir:         s.cfg.DataDir,
		Database:        databaseState,
		UptimeSeconds:   int64(time.Since(s.started).Seconds()),
		CheckedAtRFC333: time.Now().Format(time.RFC3339),
	}
}

// ServiceShutdown is the Wails v3 service lifecycle hook: it closes the
// database when the application shuts down.
func (s *AppService) ServiceShutdown() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("app service shutdown: %w", err)
	}
	return nil
}
