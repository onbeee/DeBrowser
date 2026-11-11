// Package singleinstance implements DeBrowser's application-level
// single-instance protection, scoped to one portable installation.
//
// The mechanism is an operating-system file lock, not a PID file:
//
//   - Every process opens (creating if needed) the lock file inside
//     the portable data directory (see internal/paths) and attempts
//     a non-blocking EXCLUSIVE lock on it — syscall.Flock on Unix,
//     LockFileEx on Windows.
//   - Exactly one process can hold that lock; the OS itself is the
//     arbiter, so there is no check-then-delete race: contenders
//     never remove or replace each other's lock file. The file is
//     never deleted on release for the same reason (an unlink would
//     let a newcomer lock a fresh inode while an older handle still
//     holds the unlinked one).
//   - When the holder exits — cleanly, by crash, by force kill, or
//     by power loss — the OS releases the lock automatically, so the
//     next start acquires it immediately. Stale locks cannot exist.
//   - The holder writes its PID into the file purely as information,
//     so a second launch can report WHO is running; the PID is never
//     used to decide ownership.
//
// A second launch signals the running instance to focus its window
// (SignalActivation) and exits without touching the database.
// Installations in different portable folders use different lock
// files and never interfere. Nothing here touches AppData.
package singleinstance

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrAlreadyRunning is returned (wrapped in *AlreadyRunningError)
// when a live DeBrowser instance already holds the lock.
var ErrAlreadyRunning = errors.New("singleinstance: already running")

// errLocked is the platform lock primitive's "held by someone else".
var errLocked = errors.New("singleinstance: lock held")

// AlreadyRunningError reports the PID of the running instance as
// recorded in the lock file (0 when it could not be read).
type AlreadyRunningError struct {
	PID      int
	LockFile string
}

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("singleinstance: instance already running (pid %d, lock %s)", e.PID, e.LockFile)
}

func (e *AlreadyRunningError) Unwrap() error { return ErrAlreadyRunning }

// Lock is a held single-instance OS file lock. Release is idempotent
// and only ever releases THIS process's lock — it cannot disturb a
// later owner, because it never touches the file by path.
type Lock struct {
	path string
	pid  int
	file *os.File
	once sync.Once
	err  error
}

// Path returns the lock file location.
func (l *Lock) Path() string { return l.path }

// PID returns this process's PID as recorded in the lock.
func (l *Lock) PID() int { return l.pid }

// Acquire takes the single-instance lock at lockFile, or returns an
// *AlreadyRunningError naming the current holder.
func Acquire(lockFile string) (*Lock, error) {
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("singleinstance: open lock file: %w", err)
	}
	if err := tryLock(f); err != nil {
		owner := readOwnerPID(lockFile)
		_ = f.Close()
		if errors.Is(err, errLocked) {
			return nil, &AlreadyRunningError{PID: owner, LockFile: lockFile}
		}
		return nil, fmt.Errorf("singleinstance: lock file: %w", err)
	}
	pid := os.Getpid()
	// Record our PID for diagnostics (best effort: ownership is the
	// OS lock, never this content).
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n%d\n", pid, time.Now().UnixNano())), 0)
	_ = f.Sync()
	return &Lock{path: lockFile, pid: pid, file: f}, nil
}

// Release unlocks and closes the lock file. The file itself is left
// in place (see the package comment). Safe to call more than once.
func (l *Lock) Release() error {
	l.once.Do(func() {
		if uerr := unlock(l.file); uerr != nil {
			l.err = fmt.Errorf("singleinstance: unlock: %w", uerr)
		}
		if cerr := l.file.Close(); cerr != nil && l.err == nil {
			l.err = fmt.Errorf("singleinstance: close lock file: %w", cerr)
		}
	})
	return l.err
}

// readOwnerPID best-effort reads the PID recorded in the lock file
// (0 when unreadable or corrupt). Informational only.
func readOwnerPID(lockFile string) int {
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// SignalActivation asks the running instance (the lock holder of the
// same installation) to bring its window to the foreground. It is a
// best-effort signal file consumed by the running instance.
func SignalActivation(activationFile string) error {
	content := fmt.Sprintf("%d\n%d\n", os.Getpid(), time.Now().UnixNano())
	tmp := activationFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("singleinstance: write activation signal: %w", err)
	}
	if err := os.Rename(tmp, activationFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("singleinstance: publish activation signal: %w", err)
	}
	return nil
}

// ConsumeActivation reports whether an activation signal is pending,
// removing it so each signal is handled exactly once.
func ConsumeActivation(activationFile string) (bool, error) {
	if _, err := os.Stat(activationFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("singleinstance: stat activation signal: %w", err)
	}
	if err := os.Remove(activationFile); err != nil {
		return false, fmt.Errorf("singleinstance: consume activation signal: %w", err)
	}
	return true, nil
}
