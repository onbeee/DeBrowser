//go:build !windows

package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The Unix implementation exists so the backend and its tests run
// on the development platform. Windows is the shipping target and
// has its own implementation; the semantics mirror it: identity
// is PID + creation timestamp (+ executable + command line where
// readable), graceful close is a polite termination request
// (SIGTERM), and nothing is ever force-killed by this package.

func systemLauncher() Launcher { return unixLauncher{} }

type unixLauncher struct{}

type unixProcess struct {
	pid       int
	createdAt int64
	exited    chan struct{}
	exitCode  int
	waitOnce  sync.Once
}

func (p *unixProcess) PID() int                { return p.pid }
func (p *unixProcess) CreatedAt() int64        { return p.createdAt }
func (p *unixProcess) Exited() <-chan struct{} { return p.exited }
func (p *unixProcess) ExitCode() int           { return p.exitCode }

func (p *unixProcess) watchCmd(cmd *exec.Cmd) {
	p.waitOnce.Do(func() {
		go func() {
			err := cmd.Wait()
			p.exitCode = 0
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					p.exitCode = ee.ExitCode()
				} else {
					p.exitCode = -1
				}
			}
			close(p.exited)
		}()
	})
}

// watchPoll observes a process DeBrowser did not start (a
// re-attached one): a non-child cannot be waited on, so its
// existence is polled.
func (p *unixProcess) watchPoll() {
	p.waitOnce.Do(func() {
		go func() {
			for {
				if _, err := procStat(p.pid); err != nil {
					p.exitCode = -1
					close(p.exited)
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
	})
}

func (p *unixProcess) CloseGracefully(timeout time.Duration) error {
	proc, err := os.FindProcess(p.pid)
	if err != nil {
		return ErrProcessGone
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return ErrProcessGone
		}
		return fmt.Errorf("runner: terminate %d: %w", p.pid, err)
	}
	select {
	case <-p.exited:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("runner: process %d did not exit within %s of the close request", p.pid, timeout)
	}
}

// procStat reads /proc/<pid>/stat and returns the process start
// time (field 22, clock ticks since boot) — the Unix analogue of
// the Windows creation FILETIME for identity comparison.
func procStat(pid int) (int64, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrProcessGone
		}
		return 0, fmt.Errorf("runner: read stat of %d: %w", pid, err)
	}
	// The comm field (2) is parenthesised and may itself contain
	// spaces or parentheses; everything after the LAST ')' is
	// fields 3..52, so field 22 sits at index 19 of the remainder.
	s := string(raw)
	closeParen := strings.LastIndex(s, ")")
	if closeParen < 0 {
		return 0, fmt.Errorf("runner: malformed stat of %d", pid)
	}
	fields := strings.Fields(s[closeParen+1:])
	if len(fields) <= 19 {
		return 0, fmt.Errorf("runner: short stat of %d", pid)
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("runner: parse start time of %d: %w", pid, err)
	}
	return start, nil
}

// procStatFn is the identity read Launch performs right after
// starting a child, as a variable so tests can simulate its
// failure on a real started child.
var procStatFn = procStat

func (l unixLauncher) Launch(executable string, args ...string) (Process, error) {
	cmd := exec.Command(executable, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runner: start %s: %w", executable, err)
	}
	pid := cmd.Process.Pid
	created, err := procStatFn(pid)
	if err != nil {
		// Same rule as the Windows launcher: a child that cannot
		// be tracked is terminated exactly and the failure is an
		// UntrackedChildError, never a plain failure that strands
		// a running browser.
		exited := terminateChildFn(cmd)
		return nil, &UntrackedChildError{PID: pid, ChildExited: exited,
			Err: fmt.Errorf("runner: identify started process %d: %w", pid, err)}
	}
	p := &unixProcess{pid: pid, createdAt: created, exited: make(chan struct{}), exitCode: -1}
	p.watchCmd(cmd)
	return p, nil
}

func (l unixLauncher) Inspect(pid int) (Info, error) {
	created, err := procStat(pid)
	if err != nil {
		return Info{}, err
	}
	info := Info{PID: pid, CreatedAt: created}
	if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); err == nil {
		info.ExecutablePath = exe
	}
	if raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		// Keep the NUL separators: they are the argument
		// boundaries UserDataDirArg parses on.
		info.CommandLine = string(raw)
		info.HasCommandLine = true
	}
	return info, nil
}

func (l unixLauncher) Open(pid int, createdAt int64) (Process, error) {
	created, err := procStat(pid)
	if err != nil {
		return nil, err
	}
	if created != createdAt {
		return nil, ErrIdentityMismatch
	}
	p := &unixProcess{pid: pid, createdAt: created, exited: make(chan struct{}), exitCode: -1}
	p.watchPoll()
	return p, nil
}
