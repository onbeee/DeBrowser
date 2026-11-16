//go:build windows

package service

import (
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ignoreCloseSignals is a no-op on Windows: the runner's
// graceful close there is WM_CLOSE, and the "does not react"
// scenario is covered by running the fake browser windowless.
func ignoreCloseSignals() {}

// On Windows the fake browser needs a real top-level window: the
// runner's graceful close delivers WM_CLOSE to it, exactly like a
// user closing a browser window. With DEBROWSER_FAKE_WINDOW=1 the
// fake browser creates one (a predefined STATIC window with a
// replaced window procedure) and runs a message loop; WM_CLOSE
// destroys it and ends the loop, so the process exits normally.
// Without the variable it just sleeps (used where only launch and
// external termination matter).
func fakeBrowserBlock() {
	if os.Getenv("DEBROWSER_FAKE_WINDOW") != "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	user32 := windows.NewLazySystemDLL("user32.dll")
	createWindowEx := user32.NewProc("CreateWindowExW")
	setWindowLongPtr := user32.NewProc("SetWindowLongPtrW")
	callWindowProc := user32.NewProc("CallWindowProcW")
	defWindowProc := user32.NewProc("DefWindowProcW")
	destroyWindow := user32.NewProc("DestroyWindow")
	postQuitMessage := user32.NewProc("PostQuitMessage")
	getMessage := user32.NewProc("GetMessageW")
	translateMessage := user32.NewProc("TranslateMessage")
	dispatchMessage := user32.NewProc("DispatchMessageW")

	const (
		wmClose     = 0x0010
		wmDestroy   = 0x0002
		gwlpWndProc = ^uintptr(3) // -4 as uintptr
	)
	var oldProc uintptr
	proc := syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
		switch msg {
		case wmClose:
			_, _, _ = destroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			_, _, _ = postQuitMessage.Call(0)
			return 0
		}
		if oldProc != 0 {
			r, _, _ := callWindowProc.Call(oldProc, hwnd, msg, wParam, lParam)
			return r
		}
		r, _, _ := defWindowProc.Call(hwnd, msg, wParam, lParam)
		return r
	})
	className, _ := windows.UTF16PtrFromString("STATIC")
	hwnd, _, _ := createWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), 0, 0,
		0, 0, 100, 100, 0, 0, 0, 0)
	if hwnd == 0 {
		for {
			time.Sleep(time.Hour)
		}
	}
	prev, _, _ := setWindowLongPtr.Call(hwnd, gwlpWndProc, proc)
	oldProc = prev

	type msgStruct struct {
		Hwnd    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      [2]int32
	}
	var msg msgStruct
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		_, _, _ = translateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
