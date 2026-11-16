// Package runner owns the OS-level process operations behind
// Stage 9B instance runtime: launching one browser main process
// per instance, watching it, closing it gracefully, and verifying
// that a previously recorded process is still the same process.
//
// The package manages ONLY processes DeBrowser launched (or
// re-attached to after verifying their recorded identity). It
// never enumerates or kills browsers by process name, and it
// never touches processes the user started themselves.
package runner

import (
	"errors"
	"time"
)

// ErrProcessGone reports that the process no longer exists.
var ErrProcessGone = errors.New("runner: process no longer exists")

// ErrIdentityMismatch reports that a live process exists at the
// recorded PID but is not the recorded process (its creation time
// differs): the PID was recycled by the OS.
var ErrIdentityMismatch = errors.New("runner: process identity mismatch")

// ErrAccessDenied reports that the process exists but the OS
// refused to let DeBrowser inspect it. The process's identity is
// UNCONFIRMED in this case — callers must not treat it as gone
// and must not act on it.
var ErrAccessDenied = errors.New("runner: process inspection access denied")

// ErrNoWindow reports that a process has no top-level window a
// graceful close request could be delivered to (Windows).
var ErrNoWindow = errors.New("runner: process has no window to close")

// Info is a point-in-time inspection of one process.
type Info struct {
	PID            int
	CreatedAt      int64 // platform creation timestamp (Windows FILETIME ticks; Unix /proc starttime ticks)
	ExecutablePath string
	// CommandLine is the process command line as one string (""
	// when the platform could not read it). Arguments may be
	// quoted; callers verify by looking for exact substrings,
	// never by re-splitting.
	CommandLine    string
	HasCommandLine bool
}

// Process is one tracked browser main process.
type Process interface {
	PID() int
	CreatedAt() int64
	// Exited fires exactly once when the process has exited.
	Exited() <-chan struct{}
	// ExitCode is meaningful only after Exited has fired; -1 when
	// the platform could not determine it (always the case for
	// re-attached processes on some platforms).
	ExitCode() int
	// CloseGracefully asks the process to end itself the way a
	// user closing its window would (Windows: WM_CLOSE to its
	// top-level windows; Unix: SIGTERM) and waits up to timeout
	// for the exit. A nil error means the process exited within
	// the timeout. Any other outcome — no window, timeout — is an
	// error and the process is left running: this package never
	// force-kills.
	CloseGracefully(timeout time.Duration) error
}

// Launcher starts and re-attaches browser main processes.
type Launcher interface {
	// Launch starts executable with args as a new process and
	// returns it tracked. The process is NOT tied to DeBrowser's
	// lifetime: it keeps running if DeBrowser exits.
	Launch(executable string, args ...string) (Process, error)
	// Inspect reads the current identity of a live process.
	// ErrProcessGone / ErrAccessDenied distinguish "definitely
	// dead" from "alive but unverifiable".
	Inspect(pid int) (Info, error)
	// Open re-attaches to a live process whose recorded creation
	// time matches; the returned Process can be watched and
	// closed like a launched one. ErrProcessGone,
	// ErrIdentityMismatch and ErrAccessDenied report why the
	// re-attachment was refused.
	Open(pid int, createdAt int64) (Process, error)
}

// System returns the platform Launcher.
func System() Launcher { return systemLauncher() }
