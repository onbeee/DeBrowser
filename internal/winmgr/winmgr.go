// Package winmgr owns every native window operation
// multi-instance synchronization needs: finding an instance's
// top-level browser window, reading and applying window
// geometry without stealing focus, and arranging windows on a
// chosen monitor. It is the only package that talks to the
// Win32 window APIs.
//
// Rules:
//   - A window is only ever operated on after verifying it
//     belongs to the expected process (owner PID checked at
//     the point of use; callers additionally verify process
//     creation time through the instance runtime).
//   - Geometry changes use SetWindowPos with SWP_NOACTIVATE |
//     SWP_NOZORDER | SWP_NOOWNERZORDER: they never activate a
//     window and never change Z-order or TOPMOST state.
//   - State changes (maximize / restore / full placement) go
//     through SetWindowPlacement. That API is documented to
//     show the window; whether it also moves the foreground is
//     a Windows behaviour this package does NOT claim to
//     guarantee — callers surface that limitation.
//   - Two coordinate systems are kept strictly apart:
//     WINDOWPLACEMENT rectangles are workspace coordinates and
//     only ever travel between GetWindowPlacement and
//     SetWindowPlacement; SetWindowPos takes screen
//     coordinates, sourced from GetWindowRect or monitor
//     work areas. The two are never mixed.
package winmgr

import (
	"errors"
	"fmt"
)

// ErrUnsupported marks window operations on non-Windows
// platforms.
var ErrUnsupported = errors.New("当前平台不支持窗口管理")

var (
	errNoWindow        = fmt.Errorf("未找到实例的浏览器窗口")
	errAmbiguousWindow = fmt.Errorf("实例存在多个候选窗口，无法唯一确认主窗口")
)

// Rect is a window rectangle. Which coordinate system it is in
// depends on where it came from (see package rules).
type Rect struct {
	X      int32
	Y      int32
	Width  int32
	Height int32
}

// Point is a 2D point (minimized/maximized position in a
// placement).
type Point struct {
	X int32
	Y int32
}

// ShowCmd values mirror SW_SHOWNORMAL / SW_SHOWMINIMIZED /
// SW_SHOWMAXIMIZED.
const (
	ShowNormal    = 1
	ShowMinimized = 2
	ShowMaximized = 3
)

// Placement mirrors the full Win32 WINDOWPLACEMENT state of a
// window. NormalRect is in WORKSPACE coordinates: it is only
// meaningful to SetWindowPlacement, never to SetWindowPos.
type Placement struct {
	NormalRect  Rect
	MinPosition Point
	MaxPosition Point
	ShowCmd     int32
	Flags       uint32
}

// Monitor describes one display's work area in SCREEN
// coordinates.
type Monitor struct {
	ID        int
	Primary   bool
	WorkArea  Rect
	DPI       uint32
	DeviceKey string
}

// PlacementsEqual reports whether two placements describe the
// same window state.
func PlacementsEqual(a, b Placement) bool { return a == b }

// GetPlacement reads the window's full placement.
func GetPlacement(hwnd uintptr) (Placement, error) { return getPlacement(hwnd) }

// SetPlacement applies a full placement (state + workspace
// rectangle). See the package rules for the foreground
// limitation of state changes.
func SetPlacement(hwnd uintptr, p Placement) error { return setPlacement(hwnd, p) }

// WindowScreenRect returns the window's actual rectangle in
// SCREEN coordinates (GetWindowRect).
func WindowScreenRect(hwnd uintptr) (Rect, error) { return windowScreenRect(hwnd) }

// SetWindowRect moves/resizes a window to a SCREEN-coordinate
// rectangle without activating it or changing its Z-order.
func SetWindowRect(hwnd uintptr, r Rect) error { return setWindowRect(hwnd, r) }

// WindowOwnerPID returns the PID owning the window, or 0 when
// the window is invalid.
func WindowOwnerPID(hwnd uintptr) int { return windowOwnerPID(hwnd) }

// ListMonitors enumerates displays with work areas and DPI.
func ListMonitors() ([]Monitor, error) { return listMonitors() }

// PlanWindowMirror decides how a follower window follows the
// master's window state, keeping the two coordinate systems
// apart:
//   - both normal: mirror the master's actual SCREEN rectangle
//     through the non-activating geometry path;
//   - master minimized: do nothing (minimizing is not mirrored
//     in the first version);
//   - anything else (a state change is involved): apply the
//     master's full placement through SetWindowPlacement,
//     whose foreground behaviour is a documented limitation.
func PlanWindowMirror(master Placement, masterScreen Rect, follower Placement) (screen *Rect, placement *Placement) {
	if master.ShowCmd == ShowMinimized {
		return nil, nil
	}
	if master.ShowCmd == ShowNormal && follower.ShowCmd == ShowNormal {
		r := masterScreen
		return &r, nil
	}
	p := master
	return nil, &p
}

// PlanArrange decides how one window reaches a target SCREEN
// rectangle during auto-arrange:
//   - already normal: apply the target directly as geometry;
//   - maximized/minimized: first restore the window to normal
//     using its OWN placement (workspace coordinates, never
//     the target), then apply the target as screen geometry.
func PlanArrange(current Placement, currentScreen Rect, target Rect) (restore *Placement, screen *Rect) {
	if current.ShowCmd == ShowNormal {
		if currentScreen == target {
			return nil, nil
		}
		r := target
		return nil, &r
	}
	own := current
	own.ShowCmd = ShowNormal
	r := target
	return &own, &r
}

// ArrangeRects splits a work area into near-equal tiles for n
// windows, row-major. Pure layout math shared by the service
// and its tests.
func ArrangeRects(work Rect, n int) []Rect {
	if n <= 0 {
		return nil
	}
	cols := 1
	for cols*cols < n {
		cols++
	}
	rows := (n + cols - 1) / cols
	out := make([]Rect, 0, n)
	for i := 0; i < n; i++ {
		c := int32(i % cols)
		r := int32(i / cols)
		x0 := work.X + c*work.Width/int32(cols)
		x1 := work.X + (c+1)*work.Width/int32(cols)
		y0 := work.Y + r*work.Height/int32(rows)
		y1 := work.Y + (r+1)*work.Height/int32(rows)
		out = append(out, Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0})
	}
	return out
}

// WindowInfo describes one visible top-level window of a
// process, for identity disambiguation.
type WindowInfo struct {
	HWND      uintptr
	ClassName string
	Title     string
	Area      int64
}

// ListProcessWindows returns the visible top-level windows
// owned by the process, with class and title.
func ListProcessWindows(pid int) ([]WindowInfo, error) { return listProcessWindows(pid) }

// chromiumMainWindowClass is the window class Chromium uses
// for its top-level browser windows.
const chromiumMainWindowClass = "Chrome_WidgetWin_1"

// PickMainWindow uniquely identifies a process's main browser
// window: exactly one Chromium-class top-level window wins;
// with no Chromium-class window, a single candidate wins;
// anything else is ambiguous and returns an error — the first
// window is never guessed.
func PickMainWindow(wins []WindowInfo) (uintptr, error) {
	var chromium []WindowInfo
	for _, w := range wins {
		if w.ClassName == chromiumMainWindowClass {
			chromium = append(chromium, w)
		}
	}
	switch {
	case len(chromium) == 1:
		return chromium[0].HWND, nil
	case len(chromium) > 1:
		return 0, errAmbiguousWindow
	case len(wins) == 1:
		return wins[0].HWND, nil
	case len(wins) == 0:
		return 0, errNoWindow
	default:
		return 0, errAmbiguousWindow
	}
}
