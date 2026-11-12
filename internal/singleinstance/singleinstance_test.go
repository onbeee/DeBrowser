package singleinstance

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "app.lock")
}

func TestAcquireBlocksSecondAcquireAndReleaseFrees(t *testing.T) {
	path := lockPath(t)
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// A second acquire — even from the same process on a new handle —
	// must fail: the OS lock, not PID bookkeeping, is the arbiter.
	_, err = Acquire(path)
	var already *AlreadyRunningError
	if !errors.As(err, &already) {
		t.Fatalf("second Acquire error = %v, want AlreadyRunningError", err)
	}
	if already.PID != os.Getpid() {
		t.Errorf("reported owner pid = %d, want our pid %d", already.PID, os.Getpid())
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Error("error does not wrap ErrAlreadyRunning")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Idempotent.
	if err := first.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	// After release, acquisition succeeds again immediately.
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = second.Release()
}

func TestLockFileContentIsInformationalOnly(t *testing.T) {
	path := lockPath(t)
	// A leftover file with garbage or a dead PID in it must NOT block
	// acquisition: ownership is the OS lock, never the content. This
	// is the stale-lock case from the old PID-file design.
	if err := os.WriteFile(path, []byte("999999\nstale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire over stale content: %v", err)
	}
	_ = lock.Release()
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire over corrupt content: %v", err)
	}
	_ = lock2.Release()
}

func TestDifferentDirectoriesDoNotInterfere(t *testing.T) {
	a := filepath.Join(t.TempDir(), "app.lock")
	b := filepath.Join(t.TempDir(), "app.lock")
	la, err := Acquire(a)
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	defer func() { _ = la.Release() }()
	lb, err := Acquire(b)
	if err != nil {
		t.Fatalf("Acquire B (different installation): %v", err)
	}
	_ = lb.Release()
}

func TestActivationSignalRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "activate.request")
	if ok, err := ConsumeActivation(file); err != nil || ok {
		t.Fatalf("Consume with no signal = %v, %v; want false, nil", ok, err)
	}
	if err := SignalActivation(file); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if ok, err := ConsumeActivation(file); err != nil || !ok {
		t.Fatalf("Consume after signal = %v, %v; want true, nil", ok, err)
	}
	if ok, err := ConsumeActivation(file); err != nil || ok {
		t.Errorf("second Consume = %v, %v; want false, nil (signal handled once)", ok, err)
	}
}
