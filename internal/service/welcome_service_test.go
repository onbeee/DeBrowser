package service

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/paths"
)

func welcomeTestLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatalf("resolve layout: %v", err)
	}
	return layout
}

func TestWelcomeServiceStartNewUserSuccess(t *testing.T) {
	layout := welcomeTestLayout(t)
	ready := false
	svc := NewWelcomeService(layout, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { ready = true })

	if msg := svc.StartNewUser(); msg != "" {
		t.Fatalf("StartNewUser = %q, want empty (success)", msg)
	}
	if !ready {
		t.Fatal("onReady was not invoked after successful initialization")
	}
	if got := installstate.Detect(layout); got != installstate.PhaseReady {
		t.Fatalf("Detect = %v, want PhaseReady", got)
	}
	// A duplicate call after success is a no-op success (double
	// click protection), and initialization does not run twice.
	if msg := svc.StartNewUser(); msg != "" {
		t.Fatalf("second StartNewUser = %q, want empty", msg)
	}
}

func TestWelcomeServiceStartNewUserFailure(t *testing.T) {
	layout := welcomeTestLayout(t)
	// Block directory creation so initialization fails at step one.
	if err := os.WriteFile(layout.Data, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	ready := false
	svc := NewWelcomeService(layout, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { ready = true })

	msg := svc.StartNewUser()
	if msg == "" {
		t.Fatal("StartNewUser succeeded despite a blocked data path")
	}
	if !strings.Contains(msg, "初始化") {
		t.Fatalf("message = %q, want a Chinese initialization error", msg)
	}
	if ready {
		t.Fatal("onReady invoked after a failed initialization")
	}
	if got := installstate.Detect(layout); got != installstate.PhaseUninitialized {
		t.Fatalf("Detect = %v, want PhaseUninitialized after failure", got)
	}
}
