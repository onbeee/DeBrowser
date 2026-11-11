package service

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/settings"
)

func testService(t *testing.T) *AppService {
	t.Helper()
	cfg, err := config.Load(func(key string) string {
		if key == config.EnvAppRoot {
			return t.TempDir()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc, err := NewAppService(cfg, db, slog.Default())
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return svc
}

func TestGetStatusReportsPortablePaths(t *testing.T) {
	svc := testService(t)
	st := svc.GetStatus()
	if st.Database != "ok" {
		t.Errorf("database = %q, want ok", st.Database)
	}
	if st.RootDir == "" || st.WebView2Dir != filepath.Join(st.RootDir, "webview2-data") {
		t.Errorf("unexpected paths: root=%s webview2=%s", st.RootDir, st.WebView2Dir)
	}
	if st.DatabasePath != filepath.Join(st.RootDir, "data", "debrowser.db") {
		t.Errorf("database path = %s", st.DatabasePath)
	}
	if st.DataDir != filepath.Join(st.RootDir, "data") {
		t.Errorf("data dir = %s", st.DataDir)
	}
	if st.Version == "" || st.Name != "DeBrowser" || st.Platform == "" || st.Arch == "" {
		t.Errorf("identity = %+v", st)
	}
}

func TestSettingsDefaultsAndThemePersistence(t *testing.T) {
	svc := testService(t)
	got := svc.GetSettings()
	if got.Language != settings.LanguageZhCN {
		t.Errorf("language = %q, want zh-CN", got.Language)
	}
	if got.Theme != settings.ThemeDark {
		t.Errorf("theme = %q, want dark default", got.Theme)
	}
	saved, err := svc.SaveTheme(settings.ThemeWarm)
	if err != nil {
		t.Fatalf("SaveTheme: %v", err)
	}
	if saved.Theme != settings.ThemeWarm {
		t.Errorf("saved theme = %q", saved.Theme)
	}
	// A fresh service over the same root must see the persisted theme.
	reloaded, err := settings.Load(svc.cfg.Layout.SettingsFile)
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if reloaded.Theme != settings.ThemeWarm {
		t.Errorf("persisted theme = %q, want warm", reloaded.Theme)
	}
	if _, err := svc.SaveTheme("neon"); err == nil {
		t.Error("SaveTheme accepted an unsupported theme")
	}
}
