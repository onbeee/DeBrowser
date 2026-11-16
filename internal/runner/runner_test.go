package runner

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMain doubles as the child process for the launch tests:
// when the test binary is re-executed with
// DEBROWSER_RUNNER_CHILD=1 it simply stays alive (a controllable
// stand-in for a browser main process) for
// DEBROWSER_RUNNER_CHILD_MS milliseconds (default: a minute —
// the tests end it long before that).
func TestMain(m *testing.M) {
	if os.Getenv("DEBROWSER_RUNNER_CHILD") == "1" {
		ms := 60_000
		if v, err := strconv.Atoi(os.Getenv("DEBROWSER_RUNNER_CHILD_MS")); err == nil && v > 0 {
			ms = v
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return
	}
	os.Exit(m.Run())
}

// launchChild starts the test binary as a child process through
// the platform launcher and returns the launch result.
func launchChild(t *testing.T, l Launcher, lifetimeMS int) (Process, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBROWSER_RUNNER_CHILD", "1")
	if lifetimeMS > 0 {
		t.Setenv("DEBROWSER_RUNNER_CHILD_MS", strconv.Itoa(lifetimeMS))
	}
	return l.Launch(exe)
}

// killAndWait ends a launched child from OUTSIDE the runner (as
// the user or a crash would) and waits until the tracked process
// reports the exit.
func killAndWait(t *testing.T, proc Process) {
	t.Helper()
	p, err := os.FindProcess(proc.PID())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	select {
	case <-proc.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("tracked process did not report the child's exit")
	}
}
