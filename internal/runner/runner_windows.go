//go:build windows

package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func systemLauncher() Launcher { return winLauncher{} }

type winLauncher struct{}

const (
	winProcessAccess = windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.SYNCHRONIZE
	wmClose          = 0x0010
)

type winProcess struct {
	pid       int
	createdAt int64
	handle    windows.Handle
	exited    chan struct{}
	exitCode  int32
	waitOnce  sync.Once
}

func filetimeTicks(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

func processCreatedAt(h windows.Handle) (int64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return filetimeTicks(creation), nil
}

func openProcess(pid int) (windows.Handle, error) {
	h, err := windows.OpenProcess(winProcessAccess, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, ErrProcessGone
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return 0, ErrAccessDenied
		}
		return 0, fmt.Errorf("runner: open process %d: %w", pid, err)
	}
	return h, nil
}

// watch starts the single exit watcher for the process. When cmd
// is non-nil (a process this launcher started) it is also reaped
// through cmd.Wait so no zombie handle lingers.
func (p *winProcess) watch(cmd *exec.Cmd) {
	p.waitOnce.Do(func() {
		go func() {
			_, _ = windows.WaitForSingleObject(p.handle, windows.INFINITE)
			if cmd != nil {
				_ = cmd.Wait()
			}
			var code uint32
			if err := windows.GetExitCodeProcess(p.handle, &code); err == nil {
				p.exitCode = int32(code)
			} else {
				p.exitCode = -1
			}
			close(p.exited)
			_ = windows.CloseHandle(p.handle)
		}()
	})
}

// The post-start tracking steps, as variables so tests can
// simulate their failure on a real started child.
var (
	openProcessFn      = openProcess
	processCreatedAtFn = processCreatedAt
)

func (l winLauncher) Launch(executable string, args ...string) (Process, error) {
	cmd := exec.Command(executable, args...)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runner: start %s: %w", executable, err)
	}
	pid := cmd.Process.Pid
	// Tracking MUST be established for the launch to count. If
	// it cannot be, the child this call just started is
	// terminated exactly (see terminateStartedChild) and the
	// failure is reported as an UntrackedChildError: leaving the
	// child running invisibly while reporting a plain launch
	// failure would strand a browser DeBrowser believes does not
	// exist.
	untracked := func(cause error) (Process, error) {
		exited := terminateChildFn(cmd)
		return nil, &UntrackedChildError{PID: pid, ChildExited: exited, Err: cause}
	}
	h, err := openProcessFn(pid)
	if err != nil {
		return untracked(fmt.Errorf("runner: track started process %d: %w", pid, err))
	}
	created, err := processCreatedAtFn(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return untracked(fmt.Errorf("runner: read creation time of %d: %w", pid, err))
	}
	p := &winProcess{pid: pid, createdAt: created, handle: h, exited: make(chan struct{}), exitCode: -1}
	p.watch(cmd)
	return p, nil
}

func (l winLauncher) Inspect(pid int) (Info, error) {
	h, err := openProcess(pid)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	created, err := processCreatedAt(h)
	if err != nil {
		return Info{}, fmt.Errorf("runner: read creation time of %d: %w", pid, err)
	}
	info := Info{PID: pid, CreatedAt: created}
	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
		info.ExecutablePath = windows.UTF16ToString(buf[:size])
	}
	if cmdline, ok := readCommandLine(h); ok {
		info.CommandLine = cmdline
		info.HasCommandLine = true
	}
	return info, nil
}

func (l winLauncher) Open(pid int, createdAt int64) (Process, error) {
	h, err := openProcess(pid)
	if err != nil {
		return nil, err
	}
	created, err := processCreatedAt(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("runner: read creation time of %d: %w", pid, err)
	}
	if created != createdAt {
		_ = windows.CloseHandle(h)
		return nil, ErrIdentityMismatch
	}
	p := &winProcess{pid: pid, createdAt: created, handle: h, exited: make(chan struct{}), exitCode: -1}
	p.watch(nil)
	return p, nil
}

func (p *winProcess) PID() int                { return p.pid }
func (p *winProcess) CreatedAt() int64        { return p.createdAt }
func (p *winProcess) Exited() <-chan struct{} { return p.exited }
func (p *winProcess) ExitCode() int           { return int(p.exitCode) }

// --- Graceful close: WM_CLOSE to the process's top-level windows.

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows  = user32.NewProc("EnumWindows")
	procGetWindowPID = user32.NewProc("GetWindowThreadProcessId")
	procPostMessage  = user32.NewProc("PostMessageW")
	procIsWindow     = user32.NewProc("IsWindow")

	enumMu      sync.Mutex
	enumTarget  uint32
	enumWindows []uintptr
)

var enumCallback = syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
	var pid uint32
	_, _, _ = procGetWindowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == enumTarget {
		enumWindows = append(enumWindows, hwnd)
	}
	return 1 // continue enumeration
})

func topLevelWindows(pid int) []uintptr {
	enumMu.Lock()
	defer enumMu.Unlock()
	enumTarget = uint32(pid)
	enumWindows = nil
	_, _, _ = procEnumWindows.Call(enumCallback, 0)
	out := make([]uintptr, len(enumWindows))
	copy(out, enumWindows)
	return out
}

func (p *winProcess) CloseGracefully(timeout time.Duration) error {
	windowsForProcess := topLevelWindows(p.pid)
	if len(windowsForProcess) == 0 {
		return ErrNoWindow
	}
	for _, hwnd := range windowsForProcess {
		// WM_CLOSE is the normal "user clicked X" request. A
		// browser may still show its own exit confirmation; that
		// is the user's business, not something to bypass.
		_, _, _ = procPostMessage.Call(hwnd, wmClose, 0, 0)
	}
	select {
	case <-p.exited:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("runner: process %d did not exit within %s of the close request", p.pid, timeout)
	}
}

// --- Command line reading (NtQueryInformationProcess -> PEB).
//
// Reading another process's command line on Windows goes through
// its Process Environment Block. This is a read-only inspection
// used solely to confirm that a re-attached process really is the
// browser of one specific instance (its --user-data-dir). Layout
// constants are the documented 64-bit PEB / RTL_USER_PROCESS_
// PARAMETERS offsets; on other architectures the read simply
// reports "unavailable" and callers fall back to the remaining
// identity signals.

var (
	ntdll                         = windows.NewLazySystemDLL("ntdll.dll")
	procNtQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
)

type processBasicInformation struct {
	Reserved1       uintptr
	PebBaseAddress  uintptr
	Reserved2       [2]uintptr
	UniqueProcessId uintptr
	Reserved3       uintptr
}

func readProcessMemory(h windows.Handle, addr uintptr, buf []byte) bool {
	var read uintptr
	err := windows.ReadProcessMemory(h, addr, &buf[0], uintptr(len(buf)), &read)
	return err == nil && read == uintptr(len(buf))
}

func readCommandLine(h windows.Handle) (string, bool) {
	if runtime.GOARCH != "amd64" {
		return "", false
	}
	if err := procNtQueryInformationProcess.Find(); err != nil {
		return "", false
	}
	var pbi processBasicInformation
	var retLen uint32
	status, _, _ := procNtQueryInformationProcess.Call(
		uintptr(h), 0, // ProcessBasicInformation
		uintptr(unsafe.Pointer(&pbi)), unsafe.Sizeof(pbi),
		uintptr(unsafe.Pointer(&retLen)))
	if status != 0 || pbi.PebBaseAddress == 0 {
		return "", false
	}
	// PEB.ProcessParameters lives at offset 0x20 (64-bit).
	var paramsAddr uintptr
	if !readProcessMemory(h, pbi.PebBaseAddress+0x20, unsafe.Slice((*byte)(unsafe.Pointer(&paramsAddr)), unsafe.Sizeof(paramsAddr))) {
		return "", false
	}
	if paramsAddr == 0 {
		return "", false
	}
	// RTL_USER_PROCESS_PARAMETERS.CommandLine (UNICODE_STRING) is
	// at offset 0x70 (64-bit): Length(2) MaximumLength(2) pad(4)
	// Buffer(8).
	head := make([]byte, 16)
	if !readProcessMemory(h, paramsAddr+0x70, head) {
		return "", false
	}
	length := uintptr(uint16(head[0]) | uint16(head[1])<<8)
	bufAddr := uintptr(uint64(head[8]) | uint64(head[9])<<8 | uint64(head[10])<<16 | uint64(head[11])<<24 |
		uint64(head[12])<<32 | uint64(head[13])<<40 | uint64(head[14])<<48 | uint64(head[15])<<56)
	if length == 0 || bufAddr == 0 || length > 64*1024 {
		return "", false
	}
	raw := make([]byte, length)
	if !readProcessMemory(h, bufAddr, raw) {
		return "", false
	}
	u16 := make([]uint16, length/2)
	for i := range u16 {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return windows.UTF16ToString(u16), true
}
