package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesDataDirOverride(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "DeBrowser")
	t.Setenv(EnvDataDir, dataDir)
	t.Setenv(EnvLogLevel, "debug")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DataDir != dataDir {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, dataDir)
	}
	if want := filepath.Join(dataDir, "debrowser.db"); cfg.DatabasePath != want {
		t.Errorf("DatabasePath = %q, want %q", cfg.DatabasePath, want)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	for _, dir := range []string{cfg.DataDir, cfg.ProfilesDir, cfg.LogsDir, cfg.BackupsDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("expected directory %q to exist: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

func TestLoadDefaultsLogLevelToInfo(t *testing.T) {
	t.Setenv(EnvDataDir, filepath.Join(t.TempDir(), "DeBrowser"))
	t.Setenv(EnvLogLevel, "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}
