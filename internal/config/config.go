// Package config resolves the runtime configuration: the portable
// directory layout (see internal/paths) plus process-level options.
//
// DeBrowser is portable-first. The application root is the directory
// containing the executable; data is never placed in %APPDATA% or
// %LOCALAPPDATA%. Two environment overrides exist for development and
// automated tests only:
//
//	DEBROWSER_APP_ROOT  use this directory as the application root
//	DEBROWSER_LOG_LEVEL debug|info|warn|error (default info)
package config

import (
	"fmt"
	"strings"

	"github.com/onbeee/DeBrowser/internal/paths"
)

// EnvAppRoot overrides the executable directory (development/tests).
const EnvAppRoot = "DEBROWSER_APP_ROOT"

// Config is the resolved startup configuration.
type Config struct {
	Layout   paths.Layout
	LogLevel string
}

// Load resolves the configuration: application root, portable layout,
// and directory creation with a writability check. A root that cannot
// be written to is a hard error — there is no fallback location.
func Load(getenv func(string) string) (Config, error) {
	root := strings.TrimSpace(getenv(EnvAppRoot))
	if root == "" {
		exeDir, err := paths.ExecutableDir()
		if err != nil {
			return Config{}, err
		}
		root = exeDir
	}
	layout, err := paths.Resolve(root)
	if err != nil {
		return Config{}, err
	}
	if err := layout.Ensure(); err != nil {
		return Config{}, err
	}
	cfg := Config{Layout: layout, LogLevel: "info"}
	if lv := strings.ToLower(strings.TrimSpace(getenv("DEBROWSER_LOG_LEVEL"))); lv != "" {
		switch lv {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = lv
		default:
			return Config{}, fmt.Errorf("invalid DEBROWSER_LOG_LEVEL %q", lv)
		}
	}
	return cfg, nil
}
