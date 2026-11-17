// Package wininput holds the Windows native input and window
// primitives multi-instance sync needs (Stage 10): low-level
// input capture, SendInput injection, window ownership queries,
// monitor enumeration and window arrangement geometry.
//
// The platform-neutral parts (event types, coordinate mapping,
// grid geometry) live in this file and are unit-tested on any OS.
// The Windows implementation is in wininput_windows.go; other
// platforms get stubs that report the feature as unsupported, so
// the rest of the program builds and behaves identically in CI.
package wininput

import "errors"

// ErrUnsupported reports that native input/window control is
// unavailable on this platform (DeBrowser ships for Windows; the
// stub exists so non-Windows builds and tests keep working).
var ErrUnsupported = errors.New("wininput: not supported on this platform")

// EventKind classifies a captured low-level input event.
type EventKind int

const (
	EventMouseMove EventKind = iota
	EventMouseDown
	EventMouseUp
	EventMouseWheel
	EventKeyDown
	EventKeyUp
)

// Event is one captured input event in physical screen pixels.
type Event struct {
	Kind EventKind
	X, Y int32
	// Button is "left", "right", "middle", "x1" or "x2" for mouse
	// button events, "" otherwise.
	Button string
	// ButtonsMask is the CDP-style pressed-buttons bitmask AFTER
	// this event (1 left, 2 right, 4 middle).
	ButtonsMask int
	// WheelDelta is the wheel rotation in WHEEL_DELTA units
	// (120 per notch), signed.
	WheelDelta int32
	// VK is the Windows virtual-key code for keyboard events.
	VK int
	// ScanCode is the hardware scan code for keyboard events.
	ScanCode int
	// SysKey marks Alt-context key events (WM_SYSKEY*).
	SysKey bool
}

// Rect is a rectangle in physical screen pixels.
type Rect struct {
	X, Y, Width, Height int32
}

// Empty reports a zero-size rectangle.
func (r Rect) Empty() bool { return r.Width <= 0 || r.Height <= 0 }

// Contains reports whether the point lies inside the rectangle.
func (r Rect) Contains(x, y int32) bool {
	return x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height
}

// MapPoint maps a point from the master's rectangle into the
// follower's rectangle by relative position — the coordinate
// mapping used for both native UI mirroring (window rects) and,
// with content rects, page input (after a CSS-pixel conversion
// by the caller).
func MapPoint(x, y int32, from, to Rect) (int32, int32) {
	if from.Empty() || to.Empty() {
		return x, y
	}
	rx := float64(x-from.X) / float64(from.Width)
	ry := float64(y-from.Y) / float64(from.Height)
	return to.X + int32(rx*float64(to.Width)), to.Y + int32(ry*float64(to.Height))
}

// Monitor describes one display.
type Monitor struct {
	// Rect is the full display area; WorkRect excludes the
	// taskbar and other app bars.
	Rect     Rect
	WorkRect Rect
	Primary  bool
	// ScalePercent is the DPI scaling (100 = 96 DPI).
	ScalePercent int
}

// GridRects computes a simple predictable grid layout for n
// windows inside a work area: master first (row-major), every
// window fully inside the area, near-square cells.
func GridRects(work Rect, n int) []Rect {
	if n <= 0 || work.Empty() {
		return nil
	}
	cols := 1
	for cols*cols < n {
		cols++
	}
	rows := (n + cols - 1) / cols
	out := make([]Rect, 0, n)
	cellW := work.Width / int32(cols)
	cellH := work.Height / int32(rows)
	for i := 0; i < n; i++ {
		col := int32(i % cols)
		row := int32(i / cols)
		r := Rect{
			X:      work.X + col*cellW,
			Y:      work.Y + row*cellH,
			Width:  cellW,
			Height: cellH,
		}
		// The last column/row absorbs the division remainder so
		// no window sticks out of the work area.
		if int(col) == cols-1 {
			r.Width = work.X + work.Width - r.X
		}
		if int(row) == rows-1 {
			r.Height = work.Y + work.Height - r.Y
		}
		out = append(out, r)
	}
	return out
}

// Hit describes the window under a screen point.
type Hit struct {
	// Window is the top-level window (HWND) owning the point.
	Window uintptr
	// PID is the process owning that window.
	PID uint32
	// IsContent reports the point is inside the browser's web
	// content (the Chromium render-widget child window), as
	// opposed to browser chrome (tabs, toolbar, menus).
	IsContent bool
	// ContentRect is the render widget's screen rectangle when
	// IsContent is true.
	ContentRect Rect
	// WindowRect is the top-level window's screen rectangle.
	WindowRect Rect
}

// WindowInfo describes one top-level window of a process, for
// matching a window on the master instance to its counterpart
// on a follower instance.
type WindowInfo struct {
	HWND    uintptr
	Class   string
	Owner   uintptr
	Rect    Rect
	Visible bool
}
