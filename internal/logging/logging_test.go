package logging

import (
	"log/slog"
	"os"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/paths"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		"bogus": slog.LevelInfo,
		"":      slog.LevelInfo,
	}
	for input, want := range cases {
		if got := ParseLevel(input); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestNewWritesToLogFile(t *testing.T) {
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Layout: layout, LogLevel: "info"}
	logger, closeFn, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger.Info("test message")
	if err := closeFn(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	if _, err := os.Stat(layout.LogFile); err != nil {
		t.Fatalf("expected log file at %s: %v", layout.LogFile, err)
	}
}
