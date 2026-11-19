//go:build windows

package winmgr

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shcore   = syscall.NewLazyDLL("shcore.dll")
	enumWin  = user32.NewProc("EnumWindows")
	enumMon  = user32.NewProc("EnumDisplayMonitors")
	getMon   = user32.NewProc("GetMonitorInfoW")
	getPid   = user32.NewProc("GetWindowThreadProcessId")
	isVis    = user32.NewProc("IsWindowVisible")
	getRect  = user32.NewProc("GetWindowRect")
	setPos   = user32.NewProc("SetWindowPos")
	showWin  = user32.NewProc("ShowWindowAsync")
	isZoomed = user32.NewProc("IsZoomed")
	isIconic = user32.NewProc("IsIconic")
	getDpi   = shcore.NewProc("GetDpiForMonitor")
)

const (
	swpNoActivate = 0x0010
	swpNoZOrder   = 0x0004
	swpShowWindow = 0x0040

	swRestore  = 9
	swMinimize = 6
	swMaximize = 3

	monitorDefaultToNull = 0
	mdtEffectiveDpi      = 0
)

type winRect struct {
	Left, Top, Right, Bottom int32
}

type monitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

func toRect(r winRect) Rect {
	return Rect{X: r.Left, Y: r.Top, Width: r.Right - r.Left, Height: r.Bottom - r.Top}
}

// Monitors enumerates display work areas in enumeration order,
// including monitors at negative coordinates.
func Monitors() ([]Monitor, error) {
	var monitors []Monitor
	cb := syscall.NewCallback(func(hMonitor uintptr, _ uintptr, _ *winRect, _ uintptr) uintptr {
		info := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := getMon.Call(hMonitor, uintptr(unsafe.Pointer(&info))); r == 0 {
			return 1
		}
		scale := 1.0
		var dpiX, dpiY uint32
		if r, _, _ := getDpi.Call(hMonitor, mdtEffectiveDpi,
			uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY))); r == 0 && dpiX > 0 {
			scale = float64(dpiX) / 96.0
		}
		monitors = append(monitors, Monitor{
			Index:    len(monitors),
			WorkArea: toRect(info.Work),
			Scale:    scale,
			Primary:  info.Flags&1 != 0,
		})
		return 1
	})
	if r, _, err := enumMon.Call(0, 0, cb, 0); r == 0 {
		return nil, fmt.Errorf("枚举显示器失败：%v", err)
	}
	if len(monitors) == 0 {
		return nil, fmt.Errorf("未检测到显示器")
	}
	return monitors, nil
}

// WindowForPID returns the largest visible top-level window owned
// by the process — a Chromium instance's main window.
func WindowForPID(pid int) (uintptr, error) {
	var best uintptr
	var bestArea int64
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var owner uint32
		_, _, _ = getPid.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) != pid {
			return 1
		}
		if visible, _, _ := isVis.Call(hwnd); visible == 0 {
			return 1
		}
		var r winRect
		if ok, _, _ := getRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ok == 0 {
			return 1
		}
		area := int64(r.Right-r.Left) * int64(r.Bottom-r.Top)
		if area > bestArea {
			bestArea = area
			best = hwnd
		}
		return 1
	})
	if r, _, err := enumWin.Call(cb, 0); r == 0 {
		return 0, fmt.Errorf("枚举窗口失败：%v", err)
	}
	if best == 0 {
		return 0, fmt.Errorf("未找到该实例的浏览器窗口")
	}
	return best, nil
}

// GetPlacement reads the window's normal rectangle and its
// maximized/minimized state.
func GetPlacement(hwnd uintptr) (Placement, error) {
	var r winRect
	if ok, _, err := getRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ok == 0 {
		return Placement{}, fmt.Errorf("读取窗口位置失败：%v", err)
	}
	zoomed, _, _ := isZoomed.Call(hwnd)
	iconic, _, _ := isIconic.Call(hwnd)
	return Placement{
		Rect:      toRect(r),
		Maximized: zoomed != 0,
		Minimized: iconic != 0,
	}, nil
}

// SetPlacement applies a placement. Geometry changes always pass
// SWP_NOACTIVATE: arranging or restoring a follower window never
// brings it to the foreground.
func SetPlacement(hwnd uintptr, p Placement) error {
	if p.Minimized {
		_, _, _ = showWin.Call(hwnd, swMinimize)
		return nil
	}
	// Restore first so the rectangle applies to a normal window.
	_, _, _ = showWin.Call(hwnd, swRestore)
	if ok, _, err := setPos.Call(hwnd, 0,
		uintptr(p.Rect.X), uintptr(p.Rect.Y),
		uintptr(p.Rect.Width), uintptr(p.Rect.Height),
		swpNoActivate|swpNoZOrder|swpShowWindow); ok == 0 {
		return fmt.Errorf("设置窗口位置失败：%v", err)
	}
	if p.Maximized {
		_, _, _ = showWin.Call(hwnd, swMaximize)
	}
	return nil
}
