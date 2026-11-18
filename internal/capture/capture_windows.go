//go:build windows

package capture

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
)

const (
	whMouseLL    = 14
	whKeyboardLL = 13
	wmQuit       = 0x0012

	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonDown = 0x0204
	wmRButtonUp   = 0x0205
	wmMButtonDown = 0x0207
	wmMButtonUp   = 0x0208
	wmMouseWheel  = 0x020A
	wmMouseHWheel = 0x020E
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmSysKeyDown  = 0x0104
	wmSysKeyUp    = 0x0105
)

type msllHookStruct struct {
	Pt          struct{ X, Y int32 }
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type kbdllHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// Capture lifecycle state. Exactly one hook thread may exist
// at a time: capDone is non-nil from the moment a thread is
// spawned until it has FULLY exited (hooks uninstalled), and
// StopCapture waits for that exit before returning — so a new
// capture can never overlap or overwrite the previous one.
var (
	capMu      sync.Mutex
	capHandler Handler
	capDone    chan struct{}
	capThread  uint32
)

// ErrBusy reports a StartCapture while a previous hook thread
// is still running or still exiting.
var ErrBusy = fmt.Errorf("capture: a previous capture is still active")

func buttonsMask() int {
	mask := 0
	if s, _, _ := procGetAsyncKeyState.Call(0x01); s&0x8000 != 0 {
		mask |= 1
	}
	if s, _, _ := procGetAsyncKeyState.Call(0x02); s&0x8000 != 0 {
		mask |= 2
	}
	if s, _, _ := procGetAsyncKeyState.Call(0x04); s&0x8000 != 0 {
		mask |= 4
	}
	return mask
}

func emit(ev Event) {
	capMu.Lock()
	h := capHandler
	capMu.Unlock()
	if h != nil {
		h(ev)
	}
}

func mouseCallback(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
	if nCode >= 0 {
		info := (*msllHookStruct)(lParam)
		ev := Event{X: info.Pt.X, Y: info.Pt.Y, ButtonsMask: buttonsMask()}
		switch wParam {
		case wmMouseMove:
			ev.Kind = EventMouseMove
			emit(ev)
		case wmLButtonDown:
			ev.Kind, ev.Button = EventMouseDown, "left"
			emit(ev)
		case wmLButtonUp:
			ev.Kind, ev.Button = EventMouseUp, "left"
			emit(ev)
		case wmRButtonDown:
			ev.Kind, ev.Button = EventMouseDown, "right"
			emit(ev)
		case wmRButtonUp:
			ev.Kind, ev.Button = EventMouseUp, "right"
			emit(ev)
		case wmMButtonDown:
			ev.Kind, ev.Button = EventMouseDown, "middle"
			emit(ev)
		case wmMButtonUp:
			ev.Kind, ev.Button = EventMouseUp, "middle"
			emit(ev)
		case wmMouseWheel:
			ev.Kind = EventMouseWheel
			ev.DeltaY = int32(int16(info.MouseData >> 16))
			emit(ev)
		case wmMouseHWheel:
			ev.Kind = EventMouseWheel
			ev.DeltaX = int32(int16(info.MouseData >> 16))
			emit(ev)
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
	return ret
}

func keyCallback(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
	if nCode >= 0 {
		info := (*kbdllHookStruct)(lParam)
		switch wParam {
		case wmKeyDown, wmSysKeyDown:
			emit(Event{Kind: EventKeyDown, VK: info.VkCode})
		case wmKeyUp, wmSysKeyUp:
			emit(Event{Kind: EventKeyUp, VK: info.VkCode})
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
	return ret
}

var (
	mouseCallbackPtr = syscall.NewCallback(mouseCallback)
	keyCallbackPtr   = syscall.NewCallback(keyCallback)
)

// StartCapture installs the low-level hooks on a dedicated
// message-pump thread. Observation only — nothing is injected.
// It fails with ErrBusy while a previous capture exists: the
// handler is never silently replaced.
func StartCapture(h Handler) error {
	capMu.Lock()
	if capDone != nil {
		capMu.Unlock()
		return ErrBusy
	}
	done := make(chan struct{})
	ready := make(chan error, 1)
	capDone = done
	capHandler = h
	capMu.Unlock()

	go func() {
		defer close(done)
		hookThreadMain(ready)
	}()
	if err := <-ready; err != nil {
		<-done // the thread has fully exited
		capMu.Lock()
		if capDone == done {
			capDone = nil
			capThread = 0
		}
		capHandler = nil
		capMu.Unlock()
		return err
	}
	return nil
}

func hookThreadMain(ready chan<- error) {
	// done is closed by the wrapper below when this goroutine
	// has completely finished (hooks uninstalled).
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tid, _, _ := kernel32.NewProc("GetCurrentThreadId").Call()
	hmod, _, _ := procGetModuleHandleW.Call(0)

	mh, _, merr := procSetWindowsHookExW.Call(whMouseLL, mouseCallbackPtr, hmod, 0)
	if mh == 0 {
		ready <- fmt.Errorf("capture: mouse hook: %v", merr)
		return
	}
	kh, _, kerr := procSetWindowsHookExW.Call(whKeyboardLL, keyCallbackPtr, hmod, 0)
	if kh == 0 {
		procUnhookWindowsHookEx.Call(mh)
		ready <- fmt.Errorf("capture: keyboard hook: %v", kerr)
		return
	}
	// The hooks are this thread's own locals; it uninstalls
	// exactly what it installed. Only the thread id is shared,
	// so StopCapture can post WM_QUIT.
	capMu.Lock()
	capThread = uint32(tid)
	capMu.Unlock()
	ready <- nil

	type msg struct {
		Hwnd    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      struct{ X, Y int32 }
	}
	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 || int32(ret) == -1 {
			break
		}
	}
	procUnhookWindowsHookEx.Call(mh)
	procUnhookWindowsHookEx.Call(kh)
}

// StopCapture uninstalls the hooks and WAITS until the hook
// thread has fully exited before returning: after it returns,
// no capture thread, hook or handler remains, and a new
// StartCapture may proceed. It is idempotent — stopping an
// already-stopped capture is a no-op.
func StopCapture() {
	capMu.Lock()
	done := capDone
	capHandler = nil
	capMu.Unlock()
	if done == nil {
		return
	}
	// Post WM_QUIT until the thread exits. The post is retried
	// because the thread's message queue (and its published id)
	// may not exist yet when stop races a just-started capture.
	for {
		capMu.Lock()
		tid := capThread
		capMu.Unlock()
		if tid != 0 {
			procPostThreadMessageW.Call(uintptr(tid), wmQuit, 0, 0)
		}
		select {
		case <-done:
			capMu.Lock()
			if capDone == done {
				capDone = nil
				capThread = 0
			}
			capMu.Unlock()
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// CurrentModifiers returns the live modifier-key state in CDP
// bit layout (Alt=1, Ctrl=2, Meta=4, Shift=8), read with
// GetAsyncKeyState so it reflects the physical keys right now.
func CurrentModifiers() int {
	down := func(vk uintptr) bool {
		state, _, _ := procGetAsyncKeyState.Call(vk)
		return state&0x8000 != 0
	}
	mods := 0
	if down(0x12) || down(0xA4) || down(0xA5) {
		mods |= 1
	}
	if down(0x11) || down(0xA2) || down(0xA3) {
		mods |= 2
	}
	if down(0x5B) || down(0x5C) {
		mods |= 4
	}
	if down(0x10) || down(0xA0) || down(0xA1) {
		mods |= 8
	}
	return mods
}
