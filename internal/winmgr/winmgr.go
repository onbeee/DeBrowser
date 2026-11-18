// Package winmgr manages browser windows through native Windows
// window APIs for multi-instance sync (Stage 10 v2): geometry,
// show state, monitor enumeration and hit classification.
//
// It deliberately contains NO input injection: no SendInput, no
// PostMessage input simulation, no foreground switching. Window
// changes are applied with SWP_NOACTIVATE and window placement
// calls that never steal the user's focus or move the real
// cursor — the failures of the previous SendInput-mirroring
// design (cursor drift, focus flicker) are designed out here,
// not tuned around.
package winmgr

import "errors"

// ErrUnsupported marks window management on a non-Windows build.
var ErrUnsupported = errors.New("winmgr: unsupported platform")

// Rect is a rectangle in virtual-screen physical pixels.
type Rect struct {
	X, Y          int32
	Width, Height int32
}

// Monitor is one display's geometry.
type Monitor struct {
	Rect     Rect // full display area
	WorkArea Rect // area not covered by the taskbar
	Scale    float64
	Primary  bool
}

// WindowState is a window's show state.
type WindowState string

const (
	StateNormal    WindowState = "normal"
	StateMinimized WindowState = "minimized"
	StateMaximized WindowState = "maximized"
)

// WindowInfo describes one top-level window.
type WindowInfo struct {
	HWND    uintptr
	Class   string
	Owner   uintptr // owning top-level window, 0 when unowned
	Rect    Rect
	Visible bool
	State   WindowState
}

// Hit classifies a screen point against one browser process.
type Hit struct {
	// InProcess reports the point is over a window of the
	// process at all.
	InProcess bool
	// IsContent reports the point is over the page content area
	// (the render widget), as opposed to browser chrome.
	IsContent bool
	// Window is the top-level window the point belongs to.
	Window WindowInfo
	// ContentRect is the content area's screen rectangle when
	// IsContent is set.
	ContentRect Rect
}

// GridRects divides area into n near-equal grid cells in
// row-major order. It is a pure function so layout behavior is
// unit-testable off Windows.
func GridRects(area Rect, n int) []Rect {
	if n <= 0 {
		return nil
	}
	cols := 1
	for cols*cols < n {
		cols++
	}
	rows := (n + cols - 1) / cols
	cw := area.Width / int32(cols)
	ch := area.Height / int32(rows)
	out := make([]Rect, 0, n)
	for i := 0; i < n; i++ {
		r, c := int32(i/cols), int32(i%cols)
		w, h := cw, ch
		if int(c) == cols-1 {
			w = area.Width - cw*int32(cols-1)
		}
		if int(r) == rows-1 {
			h = area.Height - ch*int32(rows-1)
		}
		out = append(out, Rect{
			X: area.X + c*cw, Y: area.Y + r*ch,
			Width: w, Height: h,
		})
	}
	return out
}

// MapPoint maps a point from the master's reference rectangle
// into the follower's, proportionally. Pure; unit-tested.
func MapPoint(x, y int32, from, to Rect) (int32, int32) {
	if from.Width <= 0 || from.Height <= 0 {
		return to.X, to.Y
	}
	nx := to.X + int32(float64(x-from.X)*float64(to.Width)/float64(from.Width))
	ny := to.Y + int32(float64(y-from.Y)*float64(to.Height)/float64(from.Height))
	return nx, ny
}
