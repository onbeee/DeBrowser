//go:build windows

package winmgr

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	user32 = syscall.NewLazyDLL("user32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procGetWindowPlacement       = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement       = user32.NewProc("SetWindowPlacement")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procSetWindowPos             = user32.NewProc("SetWindowPos")
	procEnumDisplayMonitors      = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW          = user32.NewProc("GetMonitorInfoW")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
)

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

func (r winRect) toRect() Rect {
	return Rect{X: r.Left, Y: r.Top, Width: r.Right - r.Left, Height: r.Bottom - r.Top}
}

func rectToWin(r Rect) winRect {
	return winRect{Left: r.X, Top: r.Y, Right: r.X + r.Width, Bottom: r.Y + r.Height}
}

type winPoint struct {
	X int32
	Y int32
}

// windowPlacement mirrors the Win32 WINDOWPLACEMENT structure
// EXACTLY: length, flags and show command (4 bytes each), the
// minimized and maximized points (8 bytes each) and the
// normal-position rectangle (16 bytes) — 44 bytes in total.
// GetWindowPlacement and SetWindowPlacement validate the
// length, so no extra field may ever be added here.
type windowPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    winPoint
	MaxPosition    winPoint
	NormalPosition winRect
}

// Compile-time size guard: this assignment only compiles while
// windowPlacement is exactly 44 bytes.
var _ [unsafe.Sizeof(windowPlacement{})]byte = [44]byte{}

func getPlacement(hwnd uintptr) (Placement, error) {
	wp := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	ret, _, err := procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
	if ret == 0 {
		return Placement{}, fmt.Errorf("读取窗口状态失败：%w", err)
	}
	return Placement{
		NormalRect:  wp.NormalPosition.toRect(),
		MinPosition: Point{X: wp.MinPosition.X, Y: wp.MinPosition.Y},
		MaxPosition: Point{X: wp.MaxPosition.X, Y: wp.MaxPosition.Y},
		ShowCmd:     int32(wp.ShowCmd),
		Flags:       wp.Flags,
	}, nil
}

func setPlacement(hwnd uintptr, p Placement) error {
	show := p.ShowCmd
	if show == 0 {
		show = ShowNormal
	}
	wp := windowPlacement{
		Length:         uint32(unsafe.Sizeof(windowPlacement{})),
		Flags:          p.Flags,
		ShowCmd:        uint32(show),
		MinPosition:    winPoint{X: p.MinPosition.X, Y: p.MinPosition.Y},
		MaxPosition:    winPoint{X: p.MaxPosition.X, Y: p.MaxPosition.Y},
		NormalPosition: rectToWin(p.NormalRect),
	}
	ret, _, err := procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
	if ret == 0 {
		return fmt.Errorf("设置窗口状态失败：%w", err)
	}
	return nil
}

func windowScreenRect(hwnd uintptr) (Rect, error) {
	var r winRect
	ret, _, err := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ret == 0 {
		return Rect{}, fmt.Errorf("读取窗口位置失败：%w", err)
	}
	return r.toRect(), nil
}

const (
	swpNoActivate    = 0x0010
	swpNoZOrder      = 0x0004
	swpNoOwnerZOrder = 0x0200
)

func setWindowRect(hwnd uintptr, r Rect) error {
	ret, _, err := procSetWindowPos.Call(hwnd, 0,
		uintptr(r.X), uintptr(r.Y), uintptr(r.Width), uintptr(r.Height),
		uintptr(swpNoActivate|swpNoZOrder|swpNoOwnerZOrder))
	if ret == 0 {
		return fmt.Errorf("调整窗口位置失败：%w", err)
	}
	return nil
}

func windowOwnerPID(hwnd uintptr) int {
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return int(pid)
}

type monitorInfo struct {
	Size       uint32
	Monitor    winRect
	Work       winRect
	Flags      uint32
	DeviceName [32]uint16
}

const monitorInfoPrimary = 0x1

func listMonitors() ([]Monitor, error) {
	var monitors []Monitor
	cb := syscall.NewCallback(func(hMonitor uintptr, _ uintptr, _ uintptr, _ uintptr) uintptr {
		info := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		ret, _, _ := procGetMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&info)))
		if ret == 0 {
			return 1
		}
		m := Monitor{
			ID:        len(monitors),
			Primary:   info.Flags&monitorInfoPrimary != 0,
			WorkArea:  info.Work.toRect(),
			DeviceKey: syscall.UTF16ToString(info.DeviceName[:]),
		}
		monitors = append(monitors, m)
		return 1
	})
	ret, _, err := procEnumDisplayMonitors.Call(0, 0, cb, 0)
	if ret == 0 {
		return nil, fmt.Errorf("枚举显示器失败：%w", err)
	}
	return monitors, nil
}

func listProcessWindows(pid int) ([]WindowInfo, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("进程号无效")
	}
	var out []WindowInfo
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var owner uint32
		_, _, _ = procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) != pid {
			return 1
		}
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		info := WindowInfo{HWND: hwnd}
		var classBuf [256]uint16
		n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&classBuf[0])), uintptr(len(classBuf)))
		if n > 0 {
			info.ClassName = syscall.UTF16ToString(classBuf[:n])
		}
		var titleBuf [512]uint16
		n, _, _ = procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), uintptr(len(titleBuf)))
		if n > 0 {
			info.Title = syscall.UTF16ToString(titleBuf[:n])
		}
		if r, err := windowScreenRect(hwnd); err == nil {
			info.Area = int64(r.Width) * int64(r.Height)
		}
		out = append(out, info)
		return 1
	})
	ret, _, err := procEnumWindows.Call(cb, 0)
	if ret == 0 {
		return nil, fmt.Errorf("枚举窗口失败：%w", err)
	}
	return out, nil
}
