package singleinstance

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func lockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "app.lock")
}

func aliveSet(pids ...int) func(int) bool {
	set := make(map[int]bool, len(pids))
	for _, p := range pids {
		set[p] = true
	}
	return func(pid int) bool { return set[pid] }
}

func TestAcquireAndRelease(t *testing.T) {
	path := lockPath(t)
	lock, err := AcquireWithOptions(path, Options{PID: 4242, Alive: aliveSet(4242)})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file still exists after release (err=%v)", err)
	}
	// Release is idempotent.
	if err := lock.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
	// And the lock can be taken again right away.
	lock2, err := AcquireWithOptions(path, Options{PID: 4243, Alive: aliveSet(4243)})
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = lock2.Release()
}

func TestSecondAcquireReportsAlreadyRunning(t *testing.T) {
	path := lockPath(t)
	first, err := AcquireWithOptions(path, Options{PID: 1111, Alive: aliveSet(1111, 2222)})
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer func() { _ = first.Release() }()

	_, err = AcquireWithOptions(path, Options{PID: 2222, Alive: aliveSet(1111, 2222)})
	var already *AlreadyRunningError
	if !errors.As(err, &already) {
		t.Fatalf("second Acquire error = %v, want AlreadyRunningError", err)
	}
	if already.PID != 1111 {
		t.Errorf("reported owner pid = %d, want 1111", already.PID)
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Error("error does not wrap ErrAlreadyRunning")
	}
}

func TestStaleLockFromDeadProcessIsTakenOver(t *testing.T) {
	path := lockPath(t)
	// Simulate a crashed instance: a lock file whose owner is dead.
	if err := os.WriteFile(path, []byte("999999\n123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWithOptions(path, Options{PID: 3333, Alive: aliveSet(3333)})
	if err != nil {
		t.Fatalf("Acquire over stale lock: %v", err)
	}
	defer func() { _ = lock.Release() }()
	owner, err := readOwnerPID(path)
	if err != nil || owner != 3333 {
		t.Errorf("lock owner = %d (err %v), want 3333 after takeover", owner, err)
	}
}

func TestCorruptLockIsTakenOver(t *testing.T) {
	path := lockPath(t)
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireWithOptions(path, Options{PID: 4444, Alive: aliveSet(4444)})
	if err != nil {
		t.Fatalf("Acquire over corrupt lock: %v", err)
	}
	_ = lock.Release()
}

func TestRealLivenessCheck(t *testing.T) {
	// With the platform liveness check, our own (very much alive) PID
	// in a foreign lock file must block acquisition, while a dead PID
	// must not. This exercises alive_windows.go / alive_unix.go for
	// real on whichever OS runs the test — including Windows CI.
	path := lockPath(t)
	// Our own PID is certainly alive.
	self := os.Getpid()
	content := []byte(strconv.Itoa(self) + "\n0\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Error("Acquire succeeded against a lock held by a live pid, want AlreadyRunning")
	} else {
		var already *AlreadyRunningError
		if !errors.As(err, &already) {
			t.Errorf("error = %v, want AlreadyRunningError", err)
		}
	}
	// A lock naming an implausible dead PID is stale.
	if err := os.WriteFile(path, []byte("999999\n0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire over dead-pid lock with real liveness: %v", err)
	}
	_ = lock.Release()
}

func TestConcurrentAcquireExactlyOneWinner(t *testing.T) {
	path := lockPath(t)
	const racers = 16
	alive := func(pid int) bool { return pid >= 5000 && pid < 5000+racers }
	var wg sync.WaitGroup
	results := make(chan error, racers)
	locks := make(chan *Lock, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lock, err := AcquireWithOptions(path, Options{PID: 5000 + i, Alive: alive})
			if lock != nil {
				locks <- lock
			}
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(locks)
	winners := 0
	alreadyRunning := 0
	for err := range results {
		if err == nil {
			winners++
			continue
		}
		var already *AlreadyRunningError
		if errors.As(err, &already) {
			alreadyRunning++
		} else {
			t.Errorf("unexpected acquire error: %v", err)
		}
	}
	if winners != 1 || alreadyRunning != racers-1 {
		t.Errorf("winners = %d, alreadyRunning = %d, want 1 / %d", winners, alreadyRunning, racers-1)
	}
	for lock := range locks {
		_ = lock.Release()
	}
}

func TestReleaseDoesNotRemoveForeignLock(t *testing.T) {
	path := lockPath(t)
	lock, err := AcquireWithOptions(path, Options{PID: 1111, Alive: aliveSet(1111)})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Someone else took over the file meanwhile (e.g. our file was
	// removed externally and a new instance started).
	if err := os.WriteFile(path, []byte("2222\n0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	owner, err := readOwnerPID(path)
	if err != nil || owner != 2222 {
		t.Errorf("foreign lock disturbed: owner = %d (err %v), want 2222 intact", owner, err)
	}
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

func TestDifferentDirectoriesDoNotInterfere(t *testing.T) {
	a := filepath.Join(t.TempDir(), "app.lock")
	b := filepath.Join(t.TempDir(), "app.lock")
	la, err := AcquireWithOptions(a, Options{PID: 1111, Alive: aliveSet(1111)})
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	defer func() { _ = la.Release() }()
	lb, err := AcquireWithOptions(b, Options{PID: 1111, Alive: aliveSet(1111)})
	if err != nil {
		t.Fatalf("Acquire B (different installation): %v", err)
	}
	_ = lb.Release()
}
