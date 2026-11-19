// Package winmgr is the small native window-geometry helper used
// by Stage 10 synchronization: enumerate monitor work areas, find
// an instance's top-level window by process ID, read and apply
// window placements, and compute grid arrangements. Every
// placement change is made without activating the target window —
// a follower window is never brought to the foreground.
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

// Monitor is one display's work area (the screen minus taskbars)
// plus its DPI scaling factor.
type Monitor struct {
	Index    int
	WorkArea Rect
	Scale    float64
	Primary  bool
}

// Placement is a window's restorable state: its normal rectangle
// and whether it is maximized or minimized.
type Placement struct {
	Rect      Rect
	Maximized bool
	Minimized bool
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
