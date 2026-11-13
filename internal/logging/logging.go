// Package logging sets up DeBrowser's structured logging. Since
// Stage 5B there are three categorized JSON Lines streams under
// data/logs/ — run, operation and error — each with its own
// retention and capacity policy (see system.go); run records are
// mirrored to stderr for development. Log files never contain
// browser cookies, credentials or proxy subscription URLs: audit
// records are a fixed field whitelist by construction, and every
// other record passes through a sanitizing guard handler.
package logging

import (
	"log/slog"

	"github.com/onbeee/DeBrowser/internal/config"
)

// ParseLevel converts a config log level string into a slog.Level.
// Unknown values fall back to info level.
func ParseLevel(value string) slog.Level {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// New creates the logging system for the given configuration and
// returns its application logger together with a close function.
// NewSystem is the fuller entry point (audit, statistics, export);
// New is kept for callers that only need the logger.
func New(cfg config.Config) (*slog.Logger, func() error, error) {
	sys, err := NewSystem(cfg)
	if err != nil {
		return nil, nil, err
	}
	return sys.Logger(), sys.Close, nil
}
