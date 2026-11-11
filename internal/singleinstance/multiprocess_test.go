package singleinstance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Multi-process contention tests. The helper is this same test
// binary re-executed with DEBROWSER_SI_HELPER=1: it tries to acquire
// the lock at DEBROWSER_SI_LOCK, appends the outcome to
// DEBROWSER_SI_RESULT, holds the lock for DEBROWSER_SI_HOLD_MS
// milliseconds when it wins, then exits (cleanly, or by being killed
// from the parent to simulate a crash). These are REAL OS processes
// contending for a REAL OS file lock — the same code path the
// application uses — on every platform CI runs, including Windows.

const (
	helperEnv  = "DEBROWSER_SI_HELPER"
	lockEnv    = "DEBROWSER_SI_LOCK"
	resultEnv  = "DEBROWSER_SI_RESULT"
	holdEnv    = "DEBROWSER_SI_HOLD_MS"
	readyEnv   = "DEBROWSER_SI_READY"
	resultFile = "result.txt"
)

func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process entry point")
	}
	lockFile := os.Getenv(lockEnv)
	result := os.Getenv(resultEnv)
	holdMS, _ := strconv.Atoi(os.Getenv(holdEnv))
	appendLine := func(s string) {
		f, err := os.OpenFile(result, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "helper: result file:", err)
			os.Exit(3)
		}
		fmt.Fprintf(f, "%s %d\n", s, os.Getpid())
		_ = f.Close()
	}
	lock, err := Acquire(lockFile)
	if err != nil {
		appendLine("ALREADY")
		os.Exit(0)
	}
	appendLine("ACQUIRED")
	if ready := os.Getenv(readyEnv); ready != "" {
		_ = os.WriteFile(ready, []byte("1"), 0o644)
	}
	if holdMS > 0 {
		time.Sleep(time.Duration(holdMS) * time.Millisecond)
	}
	_ = lock.Release()
	appendLine("RELEASED")
	os.Exit(0)
}

func startHelper(t *testing.T, dir string, holdMS int, ready string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.v")
	env := append(os.Environ(),
		helperEnv+"=1",
		lockEnv+"="+filepath.Join(dir, "app.lock"),
		resultEnv+"="+filepath.Join(dir, resultFile),
		holdEnv+"="+strconv.Itoa(holdMS),
	)
	if ready != "" {
		env = append(env, readyEnv+"="+ready)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	return cmd
}

func readOutcomes(t *testing.T, dir string) (acquired, already int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, resultFile))
	if err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.HasPrefix(line, "ACQUIRED"):
			acquired++
		case strings.HasPrefix(line, "ALREADY"):
			already++
		}
	}
	return acquired, already
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("file %s did not appear within 15s", path)
}

func TestMultiProcessFreshLockRace(t *testing.T) {
	dir := t.TempDir()
	const racers = 8
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := startHelper(t, dir, 1200, "")
			if err := cmd.Wait(); err != nil {
				t.Errorf("helper exit: %v", err)
			}
		}()
	}
	wg.Wait()
	acquired, already := readOutcomes(t, dir)
	if acquired != 1 || already != racers-1 {
		t.Errorf("acquired = %d, already = %d; want exactly 1 / %d", acquired, already, racers-1)
	}
}

func TestMultiProcessTakeoverAfterCleanExit(t *testing.T) {
	dir := t.TempDir()
	first := startHelper(t, dir, 400, "")
	if err := first.Wait(); err != nil {
		t.Fatalf("first helper: %v", err)
	}
	second := startHelper(t, dir, 0, "")
	if err := second.Wait(); err != nil {
		t.Fatalf("second helper: %v", err)
	}
	acquired, already := readOutcomes(t, dir)
	if acquired != 2 || already != 0 {
		t.Errorf("acquired = %d, already = %d; want 2 / 0 across sequential owners", acquired, already)
	}
}

func TestMultiProcessTakeoverRaceAfterCrash(t *testing.T) {
	dir := t.TempDir()
	// A holder that never releases on its own: the parent kills it
	// (SIGKILL-equivalent), simulating a crash / force kill. The OS
	// releases its lock; a pack of racers then contends for takeover
	// and exactly one may win — the scenario the old PID-file design
	// got wrong by deleting each other's fresh locks.
	ready := filepath.Join(dir, "ready.flag")
	holder := startHelper(t, dir, 120_000, ready)
	waitForFile(t, ready)
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("kill holder: %v", err)
	}
	_ = holder.Wait()

	const racers = 8
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := startHelper(t, dir, 800, "")
			if err := cmd.Wait(); err != nil {
				t.Errorf("racer exit: %v", err)
			}
		}()
	}
	wg.Wait()
	acquired, already := readOutcomes(t, dir)
	// acquired counts the killed holder's own ACQUIRED line too:
	// exactly one more acquisition may happen after the crash.
	if acquired != 2 || already != racers-1 {
		t.Errorf("acquired = %d, already = %d; want 2 (holder + one takeover) / %d", acquired, already, racers-1)
	}
	// And once the takeover winner has exited, the lock is free.
	final := startHelper(t, dir, 0, "")
	if err := final.Wait(); err != nil {
		t.Fatalf("final helper: %v", err)
	}
	acquired, _ = readOutcomes(t, dir)
	if acquired != 3 {
		t.Errorf("acquired = %d after winner exit, want 3", acquired)
	}
}
