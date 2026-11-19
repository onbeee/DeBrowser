//go:build windows

package winmgr

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32     = syscall.NewLazyDLL("user32.dll")
	shcore     = syscall.NewLazyDLL("shcore.dll")
	enumWin    = user32.NewProc("EnumWindows")
	enumMon    = user32.NewProc("EnumDisplayMonitors")
	getMon     = user32.NewProc("GetMonitorInfoW")
	getPid     = user32.NewProc("GetWindowThreadProcessId")
	isVis      = user32.NewProc("IsWindowVisible")
	getRect    = user32.NewProc("GetWindowRect")
	getPlcmt   = user32.NewProc("GetWindowPlacement")
	setPlcmt   = user32.NewProc("SetWindowPlacement")
	getForegnd = user32.NewProc("GetForegroundWindow")
	getDpi     = shcore.NewProc("GetDpiForMonitor")
)

const (
	// WPF_ASYNCWINDOWPLACEMENT: SetWindowPlacement posts the
	// placement request instead of sending it, so it never
	// activates the target window.
	wpfAsyncWindowPlacement = 0x0004

	monitorDefaultToNull = 0
	mdtEffectiveDpi      = 0
)

type winRect struct {
	Left, Top, Right, Bottom int32
}

type winPoint struct {
	X, Y int32
}

// windowPlacement mirrors the Win32 WINDOWPLACEMENT structure.
type windowPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    winPoint
	MaxPosition    winPoint
	NormalPosition winRect
	Device         winRect // Vista+: part of the structure
}

func toWinRect(r Rect) winRect {
	return winRect{Left: r.X, Top: r.Y, Right: r.X + r.Width, Bottom: r.Y + r.Height}
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

// ForegroundWindow returns the window currently in the
// foreground, so callers and checks can prove a placement change
// did not steal focus.
func ForegroundWindow() uintptr {
	hwnd, _, _ := getForegnd.Call()
	return hwnd
}

// GetPlacement reads the window's FULL placement through
// GetWindowPlacement: normal rectangle, minimized and maximized
// positions, show command and flags, in WINDOWPLACEMENT
// workspace coordinates (round-tripped unchanged by
// SetPlacement).
func GetPlacement(hwnd uintptr) (Placement, error) {
	var wp windowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))
	if ok, _, err := getPlcmt.Call(hwnd, uintptr(unsafe.Pointer(&wp))); ok == 0 {
		return Placement{}, fmt.Errorf("读取窗口位置失败：%v", err)
	}
	return Placement{
		NormalRect:  toRect(wp.NormalPosition),
		MinPosition: Point{X: wp.MinPosition.X, Y: wp.MinPosition.Y},
		MaxPosition: Point{X: wp.MaxPosition.X, Y: wp.MaxPosition.Y},
		ShowCmd:     wp.ShowCmd,
		Flags:       wp.Flags,
	}, nil
}

// SetPlacement applies a full placement through
// SetWindowPlacement with WPF_ASYNCWINDOWPLACEMENT. Unlike the
// previous ShowWindow(SW_RESTORE / SW_MINIMIZE / SW_MAXIMIZE)
// sequence — whose commands can activate the target or another
// window — an async placement never activates the window it
// moves, maximizes, minimizes or restores, so arranging or
// restoring a follower window never changes the user's
// foreground window. An unset ShowCmd is treated as normal.
func SetPlacement(hwnd uintptr, p Placement) error {
	showCmd := p.ShowCmd
	if showCmd == 0 {
		showCmd = ShowNormal
	}
	wp := windowPlacement{
		Length:         uint32(unsafe.Sizeof(windowPlacement{})),
		Flags:          p.Flags | wpfAsyncWindowPlacement,
		ShowCmd:        showCmd,
		MinPosition:    winPoint{X: p.MinPosition.X, Y: p.MinPosition.Y},
		MaxPosition:    winPoint{X: p.MaxPosition.X, Y: p.MaxPosition.Y},
		NormalPosition: toWinRect(p.NormalRect),
	}
	if ok, _, err := setPlcmt.Call(hwnd, uintptr(unsafe.Pointer(&wp))); ok == 0 {
		return fmt.Errorf("设置窗口位置失败：%v", err)
	}
	return nil
}
