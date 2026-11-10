// Package logging sets up DeBrowser's structured logger. Logs are written
// as JSON to a file inside the configured logs directory and mirrored to
// stderr for development. Log files never contain browser cookies,
// credentials or proxy subscription URLs.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"

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

// New creates a logger for the given configuration and returns it together
// with a close function that flushes and closes the log file.
func New(cfg config.Config) (*slog.Logger, func() error, error) {
	logPath := cfg.Layout.LogFile
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("logging: open log file %q: %w", logPath, err)
	}

	handler := slog.NewJSONHandler(io.MultiWriter(file, os.Stderr), &slog.HandlerOptions{
		Level: ParseLevel(cfg.LogLevel),
	})
	return slog.New(handler), file.Close, nil
}
