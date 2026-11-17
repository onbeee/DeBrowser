//go:build windows

package wininput

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessage          = user32.NewProc("GetMessageW")
	procPostThreadMessage   = user32.NewProc("PostThreadMessageW")
	procWindowFromPoint     = user32.NewProc("WindowFromPoint")
	procGetAncestor         = user32.NewProc("GetAncestor")
	procGetWindow           = user32.NewProc("GetWindow")
	procGetClassName        = user32.NewProc("GetClassNameW")
	procGetWindowRect       = user32.NewProc("GetWindowRect")
	procGetWindowPID        = user32.NewProc("GetWindowThreadProcessId")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop    = user32.NewProc("BringWindowToTop")
	procSendInput           = user32.NewProc("SendInput")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procShowWindow          = user32.NewProc("ShowWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procIsZoomed            = user32.NewProc("IsZoomed")
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfo      = user32.NewProc("GetMonitorInfoW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procGetCurrentThreadID  = kernel32.NewProc("GetCurrentThreadId")
	procGetDpiForMonitor    = shcore.NewProc("GetDpiForMonitor")
)

const (
	whMouseLL    = 14
	whKeyboardLL = 13

	wmQuit       = 0x0012
	wmMouseMove  = 0x0200
	wmLButtonDn  = 0x0201
	wmLButtonUp  = 0x0202
	wmRButtonDn  = 0x0204
	wmRButtonUp  = 0x0205
	wmMButtonDn  = 0x0207
	wmMButtonUp  = 0x0208
	wmMouseWheel = 0x020A
	wmXButtonDn  = 0x020B
	wmXButtonUp  = 0x020C
	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105

	gaRoot  = 2
	gwOwner = 4

	swRestore = 9

	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCxVirtualScreen = 78
	smCyVirtualScreen = 79

	// injectionTag marks SendInput events DeBrowser itself
	// injected, so the capture hooks can drop them and never
	// mirror our own mirroring.
	injectionTag = 0xDEB10
)

// ---------- capture ----------

type point struct{ X, Y int32 }

type msLLHookStruct struct {
	Pt          point
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type kbdLLHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

var (
	captureMu      sync.Mutex
	captureHandler func(Event)
	mouseHook      uintptr
	keyHook        uintptr
	hookThreadID   uint32
	hookReady      chan error
	buttonsDown    int // CDP-style mask tracked from real events
)

// StartCapture installs the low-level mouse and keyboard hooks on
// a dedicated message-pumping thread. Events whose dwExtraInfo
// carries our injection tag are dropped inside the hook, so
// injected input can never be re-captured.
func StartCapture(handler func(Event)) error {
	captureMu.Lock()
	defer captureMu.Unlock()
	if captureHandler != nil {
		return nil
	}
	captureHandler = handler
	hookReady = make(chan error, 1)
	go hookThread()
	if err := <-hookReady; err != nil {
		captureHandler = nil
		return err
	}
	return nil
}

// StopCapture removes the hooks and ends the pump thread.
func StopCapture() {
	captureMu.Lock()
	defer captureMu.Unlock()
	if hookThreadID != 0 {
		_, _, _ = procPostThreadMessage.Call(uintptr(hookThreadID), wmQuit, 0, 0)
	}
	captureHandler = nil
}

func hookThread() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	tid, _, _ := procGetCurrentThreadID.Call()
	captureMu.Lock()
	hookThreadID = uint32(tid)
	captureMu.Unlock()

	mouseCb := syscall.NewCallback(hookMouseProc)
	keyCb := syscall.NewCallback(hookKeyboardProc)
	mh, _, err := procSetWindowsHookEx.Call(whMouseLL, mouseCb, 0, 0)
	if mh == 0 {
		hookReady <- err
		return
	}
	kh, _, err := procSetWindowsHookEx.Call(whKeyboardLL, keyCb, 0, 0)
	if kh == 0 {
		_, _, _ = procUnhookWindowsHookEx.Call(mh)
		hookReady <- err
		return
	}
	mouseHook, keyHook = mh, kh
	hookReady <- nil

	var msg struct {
		Hwnd    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      point
	}
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if ret == 0 || ret == ^uintptr(0) { // WM_QUIT or error
			break
		}
	}
	_, _, _ = procUnhookWindowsHookEx.Call(mouseHook)
	_, _, _ = procUnhookWindowsHookEx.Call(keyHook)
	mouseHook, keyHook = 0, 0
	captureMu.Lock()
	hookThreadID = 0
	captureMu.Unlock()
}

func hookMouseProc(nCode int, wParam uintptr, lParam unsafe.Pointer) uintptr {
	if nCode >= 0 {
		hs := (*msLLHookStruct)(lParam)
		if hs.DwExtraInfo != injectionTag {
			ev := Event{X: hs.Pt.X, Y: hs.Pt.Y}
			switch wParam {
			case wmMouseMove:
				ev.Kind = EventMouseMove
			case wmLButtonDn:
				ev.Kind, ev.Button = EventMouseDown, "left"
				buttonsDown |= 1
			case wmLButtonUp:
				ev.Kind, ev.Button = EventMouseUp, "left"
				buttonsDown &^= 1
			case wmRButtonDn:
				ev.Kind, ev.Button = EventMouseDown, "right"
				buttonsDown |= 2
			case wmRButtonUp:
				ev.Kind, ev.Button = EventMouseUp, "right"
				buttonsDown &^= 2
			case wmMButtonDn:
				ev.Kind, ev.Button = EventMouseDown, "middle"
				buttonsDown |= 4
			case wmMButtonUp:
				ev.Kind, ev.Button = EventMouseUp, "middle"
				buttonsDown &^= 4
			case wmXButtonDn, wmXButtonUp:
				if hs.MouseData>>16 == 1 {
					ev.Button = "x1"
				} else {
					ev.Button = "x2"
				}
				if wParam == wmXButtonDn {
					ev.Kind = EventMouseDown
				} else {
					ev.Kind = EventMouseUp
				}
			case wmMouseWheel:
				ev.Kind = EventMouseWheel
				ev.WheelDelta = int32(int16(hs.MouseData >> 16))
			}
			ev.ButtonsMask = buttonsDown
			if ev.Kind != 0 || wParam == wmMouseMove {
				emit(ev)
			}
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
	return ret
}

func hookKeyboardProc(nCode int, wParam uintptr, lParam unsafe.Pointer) uintptr {
	if nCode >= 0 {
		hs := (*kbdLLHookStruct)(lParam)
		if hs.DwExtraInfo != injectionTag {
			ev := Event{VK: int(hs.VkCode), ScanCode: int(hs.ScanCode)}
			switch wParam {
			case wmKeyDown:
				ev.Kind = EventKeyDown
			case wmKeyUp:
				ev.Kind = EventKeyUp
			case wmSysKeyDown:
				ev.Kind, ev.SysKey = EventKeyDown, true
			case wmSysKeyUp:
				ev.Kind, ev.SysKey = EventKeyUp, true
			}
			if wParam == wmKeyDown || wParam == wmKeyUp || wParam == wmSysKeyDown || wParam == wmSysKeyUp {
				emit(ev)
			}
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
	return ret
}

func emit(ev Event) {
	captureMu.Lock()
	h := captureHandler
	captureMu.Unlock()
	if h != nil {
		h(ev)
	}
}

// ---------- window queries ----------

func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	_, _, _ = procGetWindowPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func className(hwnd uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func rectOf(hwnd uintptr) (Rect, bool) {
	var r struct{ Left, Top, Right, Bottom int32 }
	ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ok == 0 {
		return Rect{}, false
	}
	return Rect{X: r.Left, Y: r.Top, Width: r.Right - r.Left, Height: r.Bottom - r.Top}, true
}

// HitTest resolves the window under a screen point: its top-level
// owner window, the owning process, and whether the point is web
// content (the Chromium render-widget child) with its rectangle.
func HitTest(x, y int32) (Hit, bool) {
	hwnd, _, _ := procWindowFromPoint.Call(uintptr(uint64(uint32(x)) | uint64(uint32(y))<<32))
	if hwnd == 0 {
		return Hit{}, false
	}
	top, _, _ := procGetAncestor.Call(hwnd, gaRoot)
	if top == 0 {
		top = hwnd
	}
	hit := Hit{Window: top, PID: windowPID(top)}
	if r, ok := rectOf(top); ok {
		hit.WindowRect = r
	}
	// Content test: climb from the point window to the render
	// widget ancestor, if any.
	for w := hwnd; w != 0; {
		if className(w) == "Chrome_RenderWidgetHostHWND" {
			hit.IsContent = true
			if r, ok := rectOf(w); ok {
				hit.ContentRect = r
			}
			break
		}
		parent, _, _ := procGetAncestor.Call(w, 1 /* GA_PARENT */)
		if parent == 0 || parent == w {
			break
		}
		w = parent
	}
	return hit, true
}

// TopWindowForPID finds the instance browser's main top-level
// window: visible, unowned, largest area among the process's
// top-level windows.
func TopWindowForPID(pid uint32) (uintptr, bool) {
	var best uintptr
	var bestArea int64
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if windowPID(hwnd) != pid {
			return 1
		}
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		if r, ok := rectOf(hwnd); ok {
			if area := int64(r.Width) * int64(r.Height); area > bestArea {
				bestArea = area
				best = hwnd
			}
		}
		return 1
	})
	_, _, _ = procEnumWindows.Call(cb, 0)
	return best, best != 0
}

// WindowPID returns the process owning a window.
func WindowPID(hwnd uintptr) uint32 { return windowPID(hwnd) }

// OwnerWindow returns the window that owns hwnd (GW_OWNER), or 0.
func OwnerWindow(hwnd uintptr) uintptr {
	owner, _, _ := procGetWindow.Call(hwnd, gwOwner)
	return owner
}

// GetWindowRect returns a window's screen rectangle.
func GetWindowRect(hwnd uintptr) (Rect, bool) { return rectOf(hwnd) }

// ForegroundWindow returns the current foreground window.
func ForegroundWindow() uintptr {
	hwnd, _, _ := procGetForegroundWindow.Call()
	return hwnd
}

// ActivateWindow brings a window to the foreground.
func ActivateWindow(hwnd uintptr) bool {
	_, _, _ = procBringWindowToTop.Call(hwnd)
	ret, _, _ := procSetForegroundWindow.Call(hwnd)
	return ret != 0
}

// ---------- injection ----------

type mouseInput struct {
	DX, DY    int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type keybdInput struct {
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type inputMouse struct {
	Type uint32
	_    uint32
	Mi   mouseInput
}

type inputKey struct {
	Type uint32
	_    uint32
	Ki   keybdInput
	_    [8]byte // pad the union to MOUSEINPUT's 32 bytes
}

const (
	mouseeventfMove        = 0x0001
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
	mouseeventfXDown       = 0x0080
	mouseeventfXUp         = 0x0100
	mouseeventfWheel       = 0x0800
	mouseeventfVirtualDesk = 0x4000
	mouseeventfAbsolute    = 0x8000

	keyeventfKeyUp = 0x0002
)

// absolutePoint converts a physical screen point into the
// 0..65535 absolute coordinates SendInput expects, across the
// whole virtual desktop (multi-monitor safe).
func absolutePoint(x, y int32) (int32, int32) {
	vx, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	vy, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	vw, _, _ := procGetSystemMetrics.Call(smCxVirtualScreen)
	vh, _, _ := procGetSystemMetrics.Call(smCyVirtualScreen)
	if vw <= 1 || vh <= 1 {
		return x, y
	}
	ax := (int64(x) - int64(vx)) * 65535 / (int64(vw) - 1)
	ay := (int64(y) - int64(vy)) * 65535 / (int64(vh) - 1)
	return int32(ax), int32(ay)
}

func sendMouse(flags uint32, data uint32, x, y int32) {
	ax, ay := absolutePoint(x, y)
	in := inputMouse{Type: 0, Mi: mouseInput{
		DX: ax, DY: ay, MouseData: data, Flags: flags | mouseeventfAbsolute | mouseeventfVirtualDesk,
		ExtraInfo: injectionTag,
	}}
	_, _, _ = procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendKey(vk int, down bool) {
	var flags uint32
	if !down {
		flags = keyeventfKeyUp
	}
	in := inputKey{Type: 1, Ki: keybdInput{
		Vk: uint16(vk), Flags: flags, ExtraInfo: injectionTag,
	}}
	_, _, _ = procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// InjectMouse injects one mouse event at a screen point.
func InjectMouse(ev Event) {
	switch ev.Kind {
	case EventMouseMove:
		sendMouse(mouseeventfMove, 0, ev.X, ev.Y)
	case EventMouseDown, EventMouseUp:
		var flags uint32
		switch ev.Button {
		case "left":
			flags = map[bool]uint32{true: mouseeventfLeftDown, false: mouseeventfLeftUp}[ev.Kind == EventMouseDown]
		case "right":
			flags = map[bool]uint32{true: mouseeventfRightDown, false: mouseeventfRightUp}[ev.Kind == EventMouseDown]
		case "middle":
			flags = map[bool]uint32{true: mouseeventfMiddleDown, false: mouseeventfMiddleUp}[ev.Kind == EventMouseDown]
		default: // x1/x2
			flags = map[bool]uint32{true: mouseeventfXDown, false: mouseeventfXUp}[ev.Kind == EventMouseDown]
			data := uint32(1)
			if ev.Button == "x2" {
				data = 2
			}
			sendMouse(mouseeventfMove, 0, ev.X, ev.Y)
			sendMouse(flags, data, ev.X, ev.Y)
			return
		}
		sendMouse(mouseeventfMove, 0, ev.X, ev.Y)
		sendMouse(flags, 0, ev.X, ev.Y)
	case EventMouseWheel:
		sendMouse(mouseeventfMove, 0, ev.X, ev.Y)
		sendMouse(mouseeventfWheel, uint32(int32(ev.WheelDelta)), ev.X, ev.Y)
	}
}

// InjectKey injects one keyboard event by virtual-key code.
func InjectKey(vk int, down bool) { sendKey(vk, down) }

// ---------- monitors & arrangement ----------

type monitorInfoEx struct {
	Size       uint32
	Monitor    struct{ Left, Top, Right, Bottom int32 }
	Work       struct{ Left, Top, Right, Bottom int32 }
	Flags      uint32
	DeviceName [32]uint16
}

// ListMonitors enumerates the connected displays with their work
// areas and DPI scaling.
func ListMonitors() []Monitor {
	var out []Monitor
	cb := syscall.NewCallback(func(hmon uintptr, _ uintptr, _ uintptr, _ uintptr) uintptr {
		info := monitorInfoEx{Size: uint32(unsafe.Sizeof(monitorInfoEx{}))}
		ok, _, _ := procGetMonitorInfo.Call(hmon, uintptr(unsafe.Pointer(&info)))
		if ok == 0 {
			return 1
		}
		m := Monitor{
			Rect: Rect{X: info.Monitor.Left, Y: info.Monitor.Top,
				Width: info.Monitor.Right - info.Monitor.Left, Height: info.Monitor.Bottom - info.Monitor.Top},
			WorkRect: Rect{X: info.Work.Left, Y: info.Work.Top,
				Width: info.Work.Right - info.Work.Left, Height: info.Work.Bottom - info.Work.Top},
			Primary:      info.Flags&1 != 0,
			ScalePercent: 100,
		}
		if procGetDpiForMonitor.Find() == nil {
			var dpiX, dpiY uint32
			hr, _, _ := procGetDpiForMonitor.Call(hmon, 0,
				uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY)))
			if hr == 0 && dpiX > 0 {
				m.ScalePercent = int(dpiX) * 100 / 96
			}
		}
		out = append(out, m)
		return 1
	})
	_, _, _ = procEnumDisplayMonitors.Call(0, 0, cb, 0)
	return out
}

// WindowPlacement returns the window's current normal rectangle.
func WindowPlacement(hwnd uintptr) (Rect, bool) { return rectOf(hwnd) }

// IsMinimized reports whether the window is minimized.
func IsMinimized(hwnd uintptr) bool {
	ret, _, _ := procIsIconic.Call(hwnd)
	return ret != 0
}

// MoveWindow restores (if minimized or maximized) and moves a
// window to the given screen rectangle.
func MoveWindow(hwnd uintptr, r Rect) error {
	if ret, _, _ := procIsIconic.Call(hwnd); ret != 0 {
		_, _, _ = procShowWindow.Call(hwnd, swRestore)
	}
	if ret, _, _ := procIsZoomed.Call(hwnd); ret != 0 {
		_, _, _ = procShowWindow.Call(hwnd, swRestore)
	}
	ok, _, err := procSetWindowPos.Call(hwnd, 0,
		uintptr(r.X), uintptr(r.Y), uintptr(r.Width), uintptr(r.Height),
		0x0004 /* SWP_NOZORDER */)
	if ok == 0 {
		return err
	}
	return nil
}
