// Package service contains the application services exposed to the
// frontend through Wails bindings. Services are the only way the UI
// can reach backend state; business logic of later stages lives here
// and in dedicated domain packages, never in the app assembly package.
package service

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/settings"
	"github.com/onbeee/DeBrowser/internal/version"
)

// StatusEvent is the typed Wails event carrying Status snapshots.
const StatusEvent = "app:status"

// Status is a point-in-time snapshot of the running foundation. All
// paths come from the portable layout (see internal/paths).
type Status struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch"`
	RootDir       string `json:"rootDir"`
	DataDir       string `json:"dataDir"`
	Database      string `json:"database"` // "ok" or a short error description
	DatabasePath  string `json:"databasePath"`
	LogFile       string `json:"logFile"`
	WebView2Dir   string `json:"webView2Dir"`
	SettingsFile  string `json:"settingsFile"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	CheckedAt     string `json:"checkedAt"`
}

// AppService exposes foundation status and user settings.
type AppService struct {
	cfg       config.Config
	db        *database.DB
	log       *slog.Logger
	startedAt time.Time
	// auditor, when set, receives operation-audit records (Stage 5B).
	Auditor *logging.System

	mu       sync.Mutex
	settings settings.Settings
}

// NewAppService builds the service. The current user settings are
// loaded from the portable config directory.
func NewAppService(cfg config.Config, db *database.DB, log *slog.Logger) (*AppService, error) {
	current, err := settings.Load(cfg.Layout.SettingsFile)
	if err != nil {
		return nil, err
	}
	return &AppService{
		cfg:       cfg,
		db:        db,
		log:       log,
		startedAt: time.Now(),
		settings:  current,
	}, nil
}

// GetStatus returns a snapshot of the running application's real
// state, shown in the frontend status bar and overview page. It
// contains no business data.
func (s *AppService) GetStatus() Status {
	dbStatus := "ok"
	if err := s.db.Health(context.Background()); err != nil {
		dbStatus = err.Error()
	}
	l := s.cfg.Layout
	return Status{
		Name:          version.Name,
		Version:       version.Version,
		Platform:      runtime.GOOS,
		Arch:          runtime.GOARCH,
		RootDir:       l.Root,
		DataDir:       l.Data,
		Database:      dbStatus,
		DatabasePath:  l.DatabaseFile,
		LogFile:       l.LogFile,
		WebView2Dir:   l.WebView2,
		SettingsFile:  l.SettingsFile,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		CheckedAt:     time.Now().Format(time.RFC3339),
	}
}

// GetSettings returns the current user settings. The language is
// always Simplified Chinese (zh-CN); the theme is user-selectable.
func (s *AppService) GetSettings() settings.Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

// SaveTheme persists a new appearance theme ("dark" | "light" |
// "warm") and returns the full settings. The change is stored in
// <root>/config/settings.json so it survives restarts and travels
// with the portable folder.
// SaveTheme persists a new appearance theme (audited, Stage 5B).
func (s *AppService) SaveTheme(theme string) (settings.Settings, error) {
	start := time.Now()
	out, err := s.saveTheme(theme)
	auditOp(s.Auditor, "settings.update_theme", "settings", "", start, err)
	return out, err
}

func (s *AppService) saveTheme(theme string) (settings.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.settings
	next.Theme = theme
	if err := settings.Save(s.cfg.Layout.SettingsFile, next); err != nil {
		return settings.Settings{}, fmt.Errorf("save theme: %w", err)
	}
	s.settings = next
	s.log.Info("theme updated", "theme", theme)
	return next, nil
}

// ServiceShutdown is called by Wails during application shutdown. It
// deliberately does NOT close the database: the handle is owned by
// main (see main.run), which closes it only after the application and
// all background loops have fully stopped — closing it here could cut
// off a status emission still in flight.
func (s *AppService) ServiceShutdown() error {
	s.log.Info("app service shutting down")
	return nil
}
