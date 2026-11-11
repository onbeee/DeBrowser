// Package singleinstance implements DeBrowser's application-level
// single-instance protection, scoped to one portable installation.
//
// The lock is a file inside the portable data directory (see
// internal/paths): the first process creates it exclusively and
// records its PID; it holds the file open for its whole lifetime and
// removes it on clean exit. A second process that finds the file
// checks whether the recorded PID is still alive:
//
//   - alive   -> ErrAlreadyRunning; the second process signals the
//     running instance to focus its window (SignalActivation) and
//     exits without touching the database.
//   - dead    -> the lock is stale (crash, kill, power loss); it is
//     removed and acquisition is retried, so a crashed instance never
//     blocks the next start.
//   - corrupt -> treated as stale for the same reason.
//
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

// AlreadyRunningError reports the PID of the running instance.
type AlreadyRunningError struct {
	PID      int
	LockFile string
}

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("singleinstance: instance already running (pid %d, lock %s)", e.PID, e.LockFile)
}

func (e *AlreadyRunningError) Unwrap() error { return ErrAlreadyRunning }

// Options overrides behaviour for tests. The zero value is production
// behaviour.
type Options struct {
	// PID is the identity written into the lock file.
	PID int
	// Alive reports whether a PID currently belongs to a live
	// process. Defaults to the platform implementation.
	Alive func(pid int) bool
}

func (o Options) withDefaults() Options {
	if o.PID == 0 {
		o.PID = os.Getpid()
	}
	if o.Alive == nil {
		o.Alive = processAlive
	}
	return o
}

// Lock is a held single-instance lock. Release is idempotent.
type Lock struct {
	path string
	pid  int
	file *os.File
	once sync.Once
	err  error
}

// Path returns the lock file location.
func (l *Lock) Path() string { return l.path }

// PID returns the PID recorded in the lock.
func (l *Lock) PID() int { return l.pid }

// Acquire takes the single-instance lock at lockFile.
func Acquire(lockFile string) (*Lock, error) {
	return AcquireWithOptions(lockFile, Options{})
}

// AcquireWithOptions is Acquire with explicit options.
func AcquireWithOptions(lockFile string, opts Options) (*Lock, error) {
	opts = opts.withDefaults()
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			content := fmt.Sprintf("%d\n%d\n", opts.PID, time.Now().UnixNano())
			if _, werr := f.WriteString(content); werr != nil {
				_ = f.Close()
				_ = os.Remove(lockFile)
				return nil, fmt.Errorf("singleinstance: write lock file: %w", werr)
			}
			if serr := f.Sync(); serr != nil {
				_ = f.Close()
				_ = os.Remove(lockFile)
				return nil, fmt.Errorf("singleinstance: sync lock file: %w", serr)
			}
			return &Lock{path: lockFile, pid: opts.PID, file: f}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("singleinstance: create lock file: %w", err)
		}
		// The lock file exists: decide whether its owner is alive.
		owner, rerr := readOwnerPID(lockFile)
		if rerr == nil && opts.Alive(owner) {
			return nil, &AlreadyRunningError{PID: owner, LockFile: lockFile}
		}
		// Stale (dead owner) or unreadable/corrupt: remove and retry.
		// If another starter wins the recreate race, the next loop
		// iteration sees its live PID and reports AlreadyRunning.
		if rerr := os.Remove(lockFile); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return nil, fmt.Errorf("singleinstance: remove stale lock file: %w", rerr)
		}
	}
	return nil, fmt.Errorf("singleinstance: could not acquire lock %s after %d attempts", lockFile, maxAttempts)
}

// Release closes the lock file and removes it — but only if it still
// belongs to this process (a new owner may have taken over after an
// external removal; deleting its lock would break the protection).
func (l *Lock) Release() error {
	l.once.Do(func() {
		if cerr := l.file.Close(); cerr != nil {
			l.err = fmt.Errorf("singleinstance: close lock file: %w", cerr)
		}
		if owner, err := readOwnerPID(l.path); err == nil && owner == l.pid {
			if rerr := os.Remove(l.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				l.err = fmt.Errorf("singleinstance: remove lock file: %w", rerr)
			}
		}
	})
	return l.err
}

func readOwnerPID(lockFile string) (int, error) {
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("singleinstance: corrupt lock file %s", lockFile)
	}
	return pid, nil
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
