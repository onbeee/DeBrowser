//go:build windows

package capture

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	hookUser32   = syscall.NewLazyDLL("user32.dll")
	hookKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetWindowsHookExW    = hookUser32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx  = hookUser32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx       = hookUser32.NewProc("CallNextHookEx")
	procGetMessageW          = hookUser32.NewProc("GetMessageW")
	procPostThreadMessageW   = hookUser32.NewProc("PostThreadMessageW")
	procWindowFromPoint      = hookUser32.NewProc("WindowFromPoint")
	procGetForegroundWindowH = hookUser32.NewProc("GetForegroundWindow")
	procGetWindowThreadPID   = hookUser32.NewProc("GetWindowThreadProcessId")
	procGetCurrentThreadID   = hookKernel32.NewProc("GetCurrentThreadId")
)

const (
	whKeyboardLL = 13
	whMouseLL    = 14
	wmQuit       = 0x0012
	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105
)

type kbdLLHookStruct struct {
	VKCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type msLLHookStruct struct {
	Pt          struct{ X, Y int32 }
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type winMsg struct {
	HWND    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// windowsHookObserver installs both low-level hooks on one
// dedicated OS thread with its own message loop. The callbacks
// do the minimum: attribute the event to a window, verify the
// window's owner PID against the master PID, push to a bounded
// channel without blocking, and always pass the event on.
type windowsHookObserver struct {
	events chan RawHookEvent

	mu       sync.Mutex
	running  bool
	threadID uint32
	done     chan struct{}
}

func newHookObserver() HookObserver {
	return &windowsHookObserver{events: make(chan RawHookEvent, 256)}
}

func (o *windowsHookObserver) Events() <-chan RawHookEvent { return o.events }

func ownerPIDOf(hwnd uintptr) int {
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	_, _, _ = procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return int(pid)
}

func (o *windowsHookObserver) Start(masterPID int) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.running {
		return fmt.Errorf("观察钩子已在运行")
	}
	o.done = make(chan struct{})
	ready := make(chan error, 1)
	go o.run(masterPID, ready)
	if err := <-ready; err != nil {
		return err
	}
	o.running = true
	return nil
}

func (o *windowsHookObserver) run(masterPID int, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	var kbHook, msHook uintptr
	fail := func(err error) {
		if kbHook != 0 {
			_, _, _ = procUnhookWindowsHookEx.Call(kbHook)
		}
		if msHook != 0 {
			_, _, _ = procUnhookWindowsHookEx.Call(msHook)
		}
		ready <- err
		close(o.done)
	}

	kbProc := syscall.NewCallback(func(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
		if nCode >= 0 {
			ks := (*kbdLLHookStruct)(lParam)
			fg, _, _ := procGetForegroundWindowH.Call()
			if fg != 0 && ownerPIDOf(fg) == masterPID {
				down := wParam == wmKeyDown || wParam == wmSysKeyDown
				up := wParam == wmKeyUp || wParam == wmSysKeyUp
				if down || up {
					select {
					case o.events <- RawHookEvent{IsKey: true, KeyDown: down, VKCode: ks.VKCode, HWND: fg, At: time.Now()}:
					default: // queue full: drop the observation, never block the system hook
					}
				}
			}
		}
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
		return ret
	})
	msProc := syscall.NewCallback(func(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
		if nCode >= 0 {
			ms := (*msLLHookStruct)(lParam)
			pt := ms.Pt
			hwnd, _, _ := procWindowFromPoint.Call(uintptr(uint64(uint32(pt.X)) | uint64(uint32(pt.Y))<<32))
			if hwnd != 0 && ownerPIDOf(hwnd) == masterPID {
				select {
				case o.events <- RawHookEvent{MouseMsg: uint32(wParam), X: pt.X, Y: pt.Y, HWND: hwnd, At: time.Now()}:
				default:
				}
			}
		}
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
		return ret
	})

	h, _, err := procSetWindowsHookExW.Call(whKeyboardLL, kbProc, 0, 0)
	if h == 0 {
		fail(fmt.Errorf("安装键盘观察钩子失败：%w", err))
		return
	}
	kbHook = h
	h, _, err = procSetWindowsHookExW.Call(whMouseLL, msProc, 0, 0)
	if h == 0 {
		fail(fmt.Errorf("安装鼠标观察钩子失败：%w", err))
		return
	}
	msHook = h

	tid, _, _ := procGetCurrentThreadID.Call()
	o.mu.Lock()
	o.threadID = uint32(tid)
	o.mu.Unlock()
	ready <- nil

	var msg winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 || msg.Message == wmQuit {
			break
		}
	}
	_, _, _ = procUnhookWindowsHookEx.Call(kbHook)
	_, _, _ = procUnhookWindowsHookEx.Call(msHook)
	close(o.done)
}

func (o *windowsHookObserver) Stop() {
	o.mu.Lock()
	if !o.running {
		o.mu.Unlock()
		return
	}
	o.running = false
	tid := o.threadID
	done := o.done
	o.mu.Unlock()
	if tid != 0 {
		_, _, _ = procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
	}
	if done != nil {
		<-done
	}
}
