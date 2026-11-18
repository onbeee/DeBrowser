//go:build windows

package winmgr

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procGetWindowRect       = user32.NewProc("GetWindowRect")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procGetWindowPlacement  = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement  = user32.NewProc("SetWindowPlacement")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetClassNameW       = user32.NewProc("GetClassNameW")
	procGetWindow           = user32.NewProc("GetWindow")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procWindowFromPoint     = user32.NewProc("WindowFromPoint")
	procGetAncestor         = user32.NewProc("GetAncestor")
	procGetDpiForMonitor    = shcore.NewProc("GetDpiForMonitor")
)

const (
	gwOwner       = 4
	gaRoot        = 2
	swpNoActivate = 0x0010
	swpNoZOrder   = 0x0004
	swShowNormal  = 1
	swMinimize    = 6
	swMaximize    = 3
	swRestore     = 9
)

type winRect struct{ Left, Top, Right, Bottom int32 }

func toRect(r winRect) Rect {
	return Rect{X: r.Left, Y: r.Top, Width: r.Right - r.Left, Height: r.Bottom - r.Top}
}

type monitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

func ListMonitors() ([]Monitor, error) {
	var monitors []Monitor
	cb := syscall.NewCallback(func(h uintptr, _ uintptr, _ *winRect, _ uintptr) uintptr {
		mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(h, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return 1
		}
		m := Monitor{
			Rect:     toRect(mi.Monitor),
			WorkArea: toRect(mi.Work),
			Scale:    1,
			Primary:  mi.Flags&1 != 0,
		}
		var dpiX, dpiY uint32
		if r, _, _ := procGetDpiForMonitor.Call(h, 0, uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY))); r == 0 && dpiX > 0 {
			m.Scale = float64(dpiX) / 96.0
		}
		monitors = append(monitors, m)
		return 1
	})
	if r, _, err := procEnumDisplayMonitors.Call(0, 0, cb, 0); r == 0 {
		return nil, fmt.Errorf("winmgr: EnumDisplayMonitors: %v", err)
	}
	return monitors, nil
}

func windowClass(hwnd uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func WindowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

var procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")

func ownerWindow(hwnd uintptr) uintptr {
	owner, _, _ := procGetWindow.Call(hwnd, gwOwner)
	return owner
}

func WindowRect(hwnd uintptr) (Rect, bool) {
	var r winRect
	if ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ok == 0 {
		return Rect{}, false
	}
	return toRect(r), true
}

type windowPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    struct{ X, Y int32 }
	MaxPosition    struct{ X, Y int32 }
	NormalPosition winRect
	Device         winRect
}

func WindowStateOf(hwnd uintptr) WindowState {
	pl := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if ok, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&pl))); ok == 0 {
		return StateNormal
	}
	switch pl.ShowCmd {
	case swMinimize:
		return StateMinimized
	case swMaximize:
		return StateMaximized
	default:
		return StateNormal
	}
}

func windowInfo(hwnd uintptr) WindowInfo {
	rect, _ := WindowRect(hwnd)
	visible, _, _ := procIsWindowVisible.Call(hwnd)
	return WindowInfo{
		HWND: hwnd, Class: windowClass(hwnd), Owner: ownerWindow(hwnd),
		Rect: rect, Visible: visible != 0, State: WindowStateOf(hwnd),
	}
}

// enumTopWindows calls fn for every top-level window until fn
// returns false.
func enumTopWindows(fn func(hwnd uintptr) bool) {
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if !fn(hwnd) {
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
}

func ListWindowsForPID(pid uint32) []WindowInfo {
	var out []WindowInfo
	enumTopWindows(func(hwnd uintptr) bool {
		if WindowPID(hwnd) == pid {
			out = append(out, windowInfo(hwnd))
		}
		return true
	})
	return out
}

// TopWindowForPID returns the largest visible top-level window
// owned by the process — the browser's main window.
func TopWindowForPID(pid uint32) (WindowInfo, bool) {
	var best WindowInfo
	var bestArea int64 = -1
	for _, w := range ListWindowsForPID(pid) {
		if !w.Visible || w.Owner != 0 {
			continue
		}
		area := int64(w.Rect.Width) * int64(w.Rect.Height)
		if area > bestArea {
			best, bestArea = w, area
		}
	}
	return best, bestArea >= 0
}

// SetWindowRect moves/resizes a window WITHOUT activating it and
// without touching the cursor or the foreground window.
func SetWindowRect(hwnd uintptr, rect Rect) error {
	if st := WindowStateOf(hwnd); st != StateNormal {
		if err := SetWindowState(hwnd, StateNormal); err != nil {
			return err
		}
	}
	ok, _, err := procSetWindowPos.Call(hwnd, 0,
		uintptr(rect.X), uintptr(rect.Y), uintptr(rect.Width), uintptr(rect.Height),
		swpNoActivate|swpNoZOrder)
	if ok == 0 {
		return fmt.Errorf("winmgr: SetWindowPos: %v", err)
	}
	return nil
}

// SetWindowState changes a window's show state through window
// placement, which does not steal foreground focus.
func SetWindowState(hwnd uintptr, state WindowState) error {
	pl := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if ok, _, _ := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&pl))); ok == 0 {
		return fmt.Errorf("winmgr: GetWindowPlacement failed")
	}
	switch state {
	case StateMinimized:
		pl.ShowCmd = swMinimize
	case StateMaximized:
		pl.ShowCmd = swMaximize
	default:
		pl.ShowCmd = swRestore
	}
	if ok, _, err := procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&pl))); ok == 0 {
		return fmt.Errorf("winmgr: SetWindowPlacement: %v", err)
	}
	return nil
}

func ForegroundWindow() uintptr {
	hwnd, _, _ := procGetForegroundWindow.Call()
	return hwnd
}

// ClassifyPoint determines whether a screen point lies over the
// given process's page content area or over its browser chrome.
func ClassifyPoint(x, y int32, pid uint32) Hit {
	pt := struct{ X, Y int32 }{x, y}
	hwnd, _, _ := procWindowFromPoint.Call(uintptr(*(*uint64)(unsafe.Pointer(&pt))))
	if hwnd == 0 || WindowPID(hwnd) != pid {
		return Hit{}
	}
	// The top-level window owning the point.
	root, _, _ := procGetAncestor.Call(hwnd, gaRoot)
	hit := Hit{InProcess: true}
	if root != 0 {
		hit.Window = windowInfo(root)
	}
	// Content iff the hit window or one of its ancestors up to
	// the root is the render widget host.
	for w := hwnd; w != 0; {
		if windowClass(w) == "Chrome_RenderWidgetHostHWND" {
			if r, ok := WindowRect(w); ok {
				hit.IsContent = true
				hit.ContentRect = r
			}
			break
		}
		if w == root {
			break
		}
		parent, _, _ := procGetAncestor.Call(w, 1 /* GA_PARENT */)
		if parent == w {
			break
		}
		w = parent
	}
	return hit
}

var _ = kernel32
