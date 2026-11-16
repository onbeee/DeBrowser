//go:build windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsLaunchTracksRealChild(t *testing.T) {
	l := winLauncher{}
	proc, err := launchChild(t, l, 0)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if proc.PID() <= 0 || proc.CreatedAt() <= 0 {
		t.Fatalf("tracked identity incomplete: pid=%d created=%d", proc.PID(), proc.CreatedAt())
	}
	info, err := l.Inspect(proc.PID())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.CreatedAt != proc.CreatedAt() || info.ExecutablePath == "" {
		t.Fatalf("inspect identity = %+v, want created=%d and an executable path", info, proc.CreatedAt())
	}
	killAndWait(t, proc)
}

// requireGone polls the REAL Inspect until the process provably
// no longer exists.
func requireGone(t *testing.T, l Launcher, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := l.Inspect(pid); errors.Is(err, ErrProcessGone) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d still exists", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireUntracked asserts the launch error contract: an
// UntrackedChildError naming the started child.
func requireUntracked(t *testing.T, err error, wantExited bool) *UntrackedChildError {
	t.Helper()
	var untracked *UntrackedChildError
	if !errors.As(err, &untracked) {
		t.Fatalf("launch error = %v, want UntrackedChildError", err)
	}
	if untracked.ChildExited != wantExited {
		t.Fatalf("ChildExited = %v, want %v", untracked.ChildExited, wantExited)
	}
	if untracked.PID <= 0 {
		t.Fatalf("PID = %d, want the started child's pid", untracked.PID)
	}
	return untracked
}

// If opening the process handle fails after Start succeeded, the
// child this launch created must be terminated exactly and its
// exit confirmed.
func TestWindowsLaunchOpenFailureTerminatesChild(t *testing.T) {
	orig := openProcessFn
	openProcessFn = func(int) (windows.Handle, error) { return 0, errors.New("open denied") }
	defer func() { openProcessFn = orig }()

	l := winLauncher{}
	_, err := launchChild(t, l, 0)
	untracked := requireUntracked(t, err, true)
	requireGone(t, l, untracked.PID)
}

// Same contract when reading the creation time fails.
func TestWindowsLaunchCreatedAtFailureTerminatesChild(t *testing.T) {
	orig := processCreatedAtFn
	processCreatedAtFn = func(windows.Handle) (int64, error) { return 0, errors.New("times unavailable") }
	defer func() { processCreatedAtFn = orig }()

	l := winLauncher{}
	_, err := launchChild(t, l, 0)
	untracked := requireUntracked(t, err, true)
	requireGone(t, l, untracked.PID)
}

// The child may exit on its own between Start and the tracking
// failure: termination then confirms an already-exited process
// and the launch still reports a clean untracked failure.
func TestWindowsLaunchCleanupAfterChildSelfExit(t *testing.T) {
	orig := openProcessFn
	openProcessFn = func(int) (windows.Handle, error) {
		time.Sleep(500 * time.Millisecond) // the child (150ms) exits meanwhile
		return 0, errors.New("open denied")
	}
	defer func() { openProcessFn = orig }()

	l := winLauncher{}
	_, err := launchChild(t, l, 150)
	untracked := requireUntracked(t, err, true)
	requireGone(t, l, untracked.PID)
}

// When the termination itself cannot be confirmed, the error
// must say so (ChildExited=false) so the caller can hold the
// instance in the unknown state instead of believing the launch
// simply failed.
func TestWindowsLaunchUnconfirmedTerminationIsReported(t *testing.T) {
	origOpen := openProcessFn
	openProcessFn = func(int) (windows.Handle, error) { return 0, errors.New("open denied") }
	defer func() { openProcessFn = origOpen }()
	origTerminate := terminateChildFn
	terminateChildFn = func(*exec.Cmd) bool { return false }
	defer func() { terminateChildFn = origTerminate }()

	l := winLauncher{}
	_, err := launchChild(t, l, 0)
	untracked := requireUntracked(t, err, false)
	// The child really is still running: end it from outside so
	// the test leaves nothing behind.
	p, ferr := os.FindProcess(untracked.PID)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if kerr := p.Kill(); kerr != nil {
		t.Fatalf("cleanup kill: %v", kerr)
	}
	requireGone(t, l, untracked.PID)
}
