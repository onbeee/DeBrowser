package logging

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
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
	cfg := config.Config{LogsDir: t.TempDir(), LogLevel: "info"}
	logger, closeFn, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger.Info("test message")
	if err := closeFn(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(cfg.LogsDir, "debrowser.log"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected debrowser.log to exist, matches=%v err=%v", matches, err)
	}
}
