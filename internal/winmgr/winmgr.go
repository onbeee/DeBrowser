// Package winmgr is the small native window-geometry helper used
// by Stage 10 synchronization: enumerate monitor work areas, find
// an instance's top-level window by process ID, read and apply
// full window placements, and compute grid arrangements. Every
// placement change is made without activating the target window —
// a follower window is never brought to the foreground, the
// user's foreground window is never changed, and the user's real
// mouse is never moved.
package winmgr

import "errors"

// ErrUnsupported reports that window management is unavailable on
// this platform (DeBrowser's sync feature targets Windows 11).
var ErrUnsupported = errors.New("当前平台不支持窗口管理")

// Rect is a window or work-area rectangle in screen pixels.
type Rect struct {
	X      int32
	Y      int32
	Width  int32
	Height int32
}

// Point is a screen coordinate in pixels.
type Point struct {
	X int32
	Y int32
}

// Monitor is one display's work area (the screen minus taskbars)
// plus its DPI scaling factor.
type Monitor struct {
	Index    int
	WorkArea Rect
	Scale    float64
	Primary  bool
}

// ShowCmd values mirror the Windows WINDOWPLACEMENT showCmd
// field (SW_SHOWNORMAL / SW_SHOWMINIMIZED / SW_SHOWMAXIMIZED).
const (
	ShowNormal    uint32 = 1
	ShowMinimized uint32 = 2
	ShowMaximized uint32 = 3
)

// Placement is a window's FULL restorable state, mirroring the
// Windows WINDOWPLACEMENT structure: the normal (restored)
// rectangle, the minimized and maximized positions, and the
// current show command. Coordinates follow the WINDOWPLACEMENT
// workspace-coordinate rules and round-trip unchanged between
// GetPlacement and SetPlacement. A bare rectangle plus two
// booleans is NOT a placement: restoring from one loses the
// minimized/maximized positions and the exact show state.
type Placement struct {
	NormalRect  Rect
	MinPosition Point
	MaxPosition Point
	ShowCmd     uint32
	Flags       uint32
}

// PlacementsEqual reports whether two placements describe the
// same window state for mirroring purposes: same show command
// and same normal rectangle. (Min/max positions are restored
// faithfully by SetPlacement but do not by themselves constitute
// a visible change worth mirroring.)
func PlacementsEqual(a, b Placement) bool {
	return a.ShowCmd == b.ShowCmd && a.NormalRect == b.NormalRect
}

// MirrorPlacement applies the master's placement to a follower
// window, with the mirroring boundary: minimizing is never
// mirrored (a follower the user did not minimize must not be
// hidden by sync). It reports whether a change was applied. The
// apply itself never activates the follower window.
func MirrorPlacement(hwnd uintptr, master Placement) (bool, error) {
	if master.ShowCmd == ShowMinimized {
		return false, nil
	}
	current, err := GetPlacement(hwnd)
	if err != nil {
		return false, err
	}
	if PlacementsEqual(current, master) {
		return false, nil
	}
	if err := SetPlacement(hwnd, master); err != nil {
		return false, err
	}
	return true, nil
}

// GridRects divides a work area into n near-equal tiles in
// row-major order. It is pure arithmetic and platform independent.
func GridRects(work Rect, n int) []Rect {
	if n <= 0 {
		return nil
	}
	cols := 1
	for cols*cols < n {
		cols++
	}
	// Prefer a wider grid on wide work areas.
	if work.Width > work.Height && cols*cols >= n && n > 1 {
		for (cols-1)*(cols-1) >= n {
			cols--
		}
	}
	rows := (n + cols - 1) / cols
	out := make([]Rect, 0, n)
	for i := 0; i < n; i++ {
		col := int32(i % cols)
		row := int32(i / cols)
		x0 := work.X + col*work.Width/int32(cols)
		x1 := work.X + (col+1)*work.Width/int32(cols)
		y0 := work.Y + row*work.Height/int32(rows)
		y1 := work.Y + (row+1)*work.Height/int32(rows)
		out = append(out, Rect{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0})
	}
	return out
}
