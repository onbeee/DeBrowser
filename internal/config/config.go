// Package config resolves DeBrowser's local directories and runtime
// settings. DeBrowser is local-first: all application data lives under a
// single per-user data directory, never inside the repository.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment overrides.
const (
	EnvDataDir  = "DEBROWSER_DATA_DIR"
	EnvLogLevel = "DEBROWSER_LOG_LEVEL"
)

// Config holds the resolved runtime configuration.
type Config struct {
	// DataDir is the root directory for all DeBrowser application data.
	DataDir string
	// DatabasePath is the SQLite database file inside DataDir.
	DatabasePath string
	// ProfilesDir holds one isolated user data directory per browser
	// instance (created by later stages, one sub-directory per instance).
	ProfilesDir string
	// LogsDir holds structured application log files.
	LogsDir string
	// BackupsDir holds database and configuration backups.
	BackupsDir string
	// LogLevel is one of "debug", "info", "warn" or "error".
	LogLevel string
}

// Load resolves the configuration from the environment and the operating
// system's per-user config location, creating the data directories when
// they do not exist yet.
//
// On Windows the default data directory is %AppData%\DeBrowser.
func Load() (Config, error) {
	dataDir := strings.TrimSpace(os.Getenv(EnvDataDir))
	if dataDir == "" {
		userConfigDir, err := os.UserConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("config: resolve user config dir: %w", err)
		}
		dataDir = filepath.Join(userConfigDir, "DeBrowser")
	}

	cfg := Config{
		DataDir:      dataDir,
		DatabasePath: filepath.Join(dataDir, "debrowser.db"),
		ProfilesDir:  filepath.Join(dataDir, "profiles"),
		LogsDir:      filepath.Join(dataDir, "logs"),
		BackupsDir:   filepath.Join(dataDir, "backups"),
		LogLevel:     strings.ToLower(strings.TrimSpace(os.Getenv(EnvLogLevel))),
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	for _, dir := range []string{cfg.DataDir, cfg.ProfilesDir, cfg.LogsDir, cfg.BackupsDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return Config{}, fmt.Errorf("config: create data directory %q: %w", dir, err)
		}
	}
	return cfg, nil
}
