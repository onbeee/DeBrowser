package welcomerun

import (
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/singleinstance"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg, err := config.Resolve(func(key string) string {
		if key == config.EnvAppRoot {
			return root
		}
		return ""
	})
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	return cfg
}

func workspaceOf(t *testing.T, cfg config.Config) installstate.Workspace {
	t.Helper()
	ws, err := installstate.TempWorkspace(cfg.Layout.Root)
	if err != nil {
		t.Fatalf("TempWorkspace: %v", err)
	}
	return ws
}

// The core ownership scenario: a second welcome launch must signal
// the first, exit successfully, and leave the first process's
// workspace and lock completely untouched; the workspace is removed
// only when the owning process exits.
func TestSecondWelcomeLaunchLeavesOwnerWorkspaceUntouched(t *testing.T) {
	cfg := testConfig(t)
	ws := workspaceOf(t, cfg)

	started := make(chan struct{})
	release := make(chan struct{})
	ownerErr := make(chan error, 1)
	go func() {
		ownerErr <- Run(cfg, func(_ installstate.Workspace, _ *slog.Logger) error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("owner welcome did not start")
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("owner workspace missing while running: %v", err)
	}

	// Second launch: its serve function must never run, Run returns
	// nil (activation signaled instead).
	serveCalled := false
	if err := Run(cfg, func(_ installstate.Workspace, _ *slog.Logger) error {
		serveCalled = true
		return nil
	}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if serveCalled {
		t.Fatal("second launch served a window although a welcome instance was running")
	}

	// The second launch signaled activation...
	if _, err := os.Stat(ws.ActivationFile); err != nil {
		t.Fatalf("activation signal file missing after second launch: %v", err)
	}
	// ...and the owner's workspace is still there, untouched.
	if _, err := os.Stat(ws.Dir); err != nil {
		t.Fatalf("second launch removed the owner's workspace: %v", err)
	}
	if _, err := os.Stat(ws.WebView2); err != nil {
		t.Fatalf("second launch removed the owner's WebView2 data: %v", err)
	}
	// The owner still holds a valid lock: a direct acquire attempt
	// must be rejected as already-running.
	if _, err := singleinstance.Acquire(ws.LockFile); err == nil {
		t.Fatal("owner lock was lost after the second launch")
	} else {
		var already *singleinstance.AlreadyRunningError
		if !errors.As(err, &already) {
			t.Fatalf("direct acquire error = %v, want AlreadyRunningError", err)
		}
	}

	// Only when the owner exits is the workspace cleaned up.
	close(release)
	select {
	case err := <-ownerErr:
		if err != nil {
			t.Fatalf("owner Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("owner welcome did not exit")
	}
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after owner exit (err=%v)", err)
	}
	// And the lock is free again: a fresh launch recreates the
	// workspace (TempWorkspace) and can acquire its lock.
	ws = workspaceOf(t, cfg)
	defer func() { _ = os.RemoveAll(ws.Dir) }()
	lock, err := singleinstance.Acquire(ws.LockFile)
	if err != nil {
		t.Fatalf("acquire after owner exit: %v", err)
	}
	_ = lock.Release()
}

// A serve failure still cleans up the owned workspace and frees the
// lock, and the error is propagated to the caller.
func TestWelcomeServeErrorCleansUp(t *testing.T) {
	cfg := testConfig(t)
	ws := workspaceOf(t, cfg)
	boom := errors.New("window failed")
	err := Run(cfg, func(_ installstate.Workspace, _ *slog.Logger) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want the serve error", err)
	}
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after failed serve (err=%v)", err)
	}
	// The lock is free: a fresh launch recreates the workspace and
	// can acquire it.
	ws = workspaceOf(t, cfg)
	defer func() { _ = os.RemoveAll(ws.Dir) }()
	lock, err := singleinstance.Acquire(ws.LockFile)
	if err != nil {
		t.Fatalf("acquire after failed serve: %v", err)
	}
	_ = lock.Release()
}

// A clean, immediate welcome exit removes the workspace.
func TestWelcomeCleanExitRemovesWorkspace(t *testing.T) {
	cfg := testConfig(t)
	ws := workspaceOf(t, cfg)
	ran := false
	err := Run(cfg, func(_ installstate.Workspace, log *slog.Logger) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !ran {
		t.Fatal("serve was not called")
	}
	if _, err := os.Stat(ws.Dir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after clean exit (err=%v)", err)
	}
}
