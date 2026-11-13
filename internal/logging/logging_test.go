package logging

import (
	"log/slog"
	"os"
	"strings"
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
	// Stage 5B: run records live in the dated run-category file
	// inside the logs directory.
	entries, err := os.ReadDir(layout.Logs)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if len(e.Name()) > 4 && e.Name()[:4] == "run-" {
			found = true
			data, err := os.ReadFile(layout.Logs + "/" + e.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "test message") {
				t.Fatalf("run log %s does not contain the test message", e.Name())
			}
		}
	}
	if !found {
		t.Fatalf("no run-* log file in %s", layout.Logs)
	}
}
