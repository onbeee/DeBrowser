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
	"fmt"
	"os/exec"
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
	// when the platform could not read it). Callers extract
	// arguments with UserDataDirArg, which parses the command
	// line properly; a substring search is never a valid
	// identity check.
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

// UntrackedChildError reports a launch whose process was
// started successfully but could not be brought under tracking
// (its handle could not be opened, or its creation time could
// not be read). Because an untracked browser would otherwise
// keep running invisibly, Launch terminates exactly the child it
// started before returning this error; ChildExited reports
// whether that termination was CONFIRMED. When ChildExited is
// false the child may still be running, and the caller must
// treat the instance as unverifiable — never as simply stopped.
type UntrackedChildError struct {
	PID         int
	ChildExited bool
	Err         error
}

func (e *UntrackedChildError) Error() string {
	if e.ChildExited {
		return fmt.Sprintf("runner: started process %d could not be tracked and was terminated: %v", e.PID, e.Err)
	}
	return fmt.Sprintf("runner: started process %d could not be tracked and its exit could not be confirmed: %v", e.PID, e.Err)
}

func (e *UntrackedChildError) Unwrap() error { return e.Err }

// terminateChildTimeout bounds the wait for a terminated
// untracked child to exit.
const terminateChildTimeout = 5 * time.Second

// terminateStartedChild ends exactly the process cmd started and
// reports whether its exit was confirmed. The termination goes
// through cmd's own process handle — the precise child of this
// launch, never a name or PID lookup — so it cannot touch the
// user's own browsers or another instance's process. A child
// that already exited on its own also counts as confirmed.
var terminateChildFn = func(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	// Kill targets the exact started process; its error is
	// expected (and harmless) when the child already exited.
	_ = cmd.Process.Kill()
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(terminateChildTimeout):
		return false
	}
}

// System returns the platform Launcher.
func System() Launcher { return systemLauncher() }
