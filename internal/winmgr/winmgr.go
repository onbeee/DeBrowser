// Package winmgr is the small native window-geometry helper used
// by Stage 10 synchronization: enumerate monitor work areas, find
// an instance's top-level window by process ID, read and apply
// window placements, and compute grid arrangements.
//
// Non-activation is handled honestly, in two tiers:
//
//   - Geometry-only changes (move / resize of a window that
//     stays in the normal show state) go through SetWindowPos
//     with SWP_NOACTIVATE | SWP_NOZORDER, which by the Windows
//     API contract does not activate the window and does not
//     change the Z order: the user's foreground window is not
//     changed by these operations.
//   - Show-state changes (maximize / restore / minimize and
//     full placement restores that include them) have no
//     Windows API that promises the foreground window can never
//     change in every situation. They are applied through
//     SetWindowPlacement and are a documented limitation, NOT
//     an absolute non-activation guarantee.
//
// The user's real mouse is never moved by any operation here.
//
// Coordinate discipline (kept strict, never mixed):
//   - WINDOWPLACEMENT coordinates (Placement.NormalRect and the
//     min/max positions) are WORKSPACE coordinates. They are
//     only ever read from GetWindowPlacement and written back
//     through SetWindowPlacement.
//   - SetWindowPos coordinates are SCREEN coordinates.
//     SetWindowRect accepts screen coordinates only — a
//     window's actual screen rectangle comes from
//     GetWindowRect, never from a placement.
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
// current show command. Its coordinates are WORKSPACE
// coordinates: they round-trip unchanged between GetPlacement
// and SetPlacement and must never be handed to SetWindowRect,
// which takes screen coordinates. A bare rectangle plus two
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

// mirrorAction is the planned way to mirror one master window
// state onto one follower window, keeping the two coordinate
// systems apart: geometry changes are planned in SCREEN
// coordinates, state transitions as a full WINDOWPLACEMENT.
type mirrorAction int

const (
	mirrorNoop mirrorAction = iota
	// mirrorSetRect applies rect (screen coordinates) through
	// SetWindowRect / SetWindowPos.
	mirrorSetRect
	// mirrorSetPlacement applies placement (workspace
	// coordinates) through SetPlacement / SetWindowPlacement.
	mirrorSetPlacement
)

// planWindowMirror decides how a follower window mirrors the
// master, from the follower's current placement and (when both
// windows are in the normal state) its actual screen rectangle,
// plus the master's show command, its actual screen rectangle
// (from GetWindowRect) and its full placement. It is pure
// decision logic; the coordinate discipline is:
//   - master normal + follower normal: geometry only, planned
//     in screen coordinates — the returned rect is the master's
//     SCREEN rectangle verbatim, never its placement rectangle.
//   - any show-state difference, or a stored-state difference
//     while both windows share a non-normal state: a full
//     placement transition, planned with the master's PLACEMENT
//     verbatim (workspace coordinates stay inside the placement
//     system).
//   - master minimized: never mirrored (documented boundary).
func planWindowMirror(follower Placement, followerScreen Rect, masterShowCmd uint32, masterScreen Rect, masterPlacement Placement) (mirrorAction, Rect, Placement) {
	if masterShowCmd == ShowMinimized {
		return mirrorNoop, Rect{}, Placement{}
	}
	if masterShowCmd == ShowNormal && follower.ShowCmd == ShowNormal {
		if followerScreen == masterScreen {
			return mirrorNoop, Rect{}, Placement{}
		}
		return mirrorSetRect, masterScreen, Placement{}
	}
	if PlacementsEqual(follower, masterPlacement) {
		return mirrorNoop, Rect{}, Placement{}
	}
	return mirrorSetPlacement, Rect{}, masterPlacement
}

// MirrorPlacement mirrors the master window's current state
// onto a follower window: masterShowCmd and masterPlacement
// come from the master's GetWindowPlacement, masterScreen is
// the master's ACTUAL screen rectangle from GetWindowRect.
// Geometry changes are applied from the screen rectangle
// through the non-activating SetWindowPos path; state
// transitions are applied as a full placement and carry the
// documented limitation — no absolute foreground guarantee is
// claimed for them. Minimizing is never mirrored. It reports
// whether a change was applied.
func MirrorPlacement(hwnd uintptr, masterShowCmd uint32, masterScreen Rect, masterPlacement Placement) (bool, error) {
	if masterShowCmd == ShowMinimized {
		return false, nil
	}
	current, err := GetPlacement(hwnd)
	if err != nil {
		return false, err
	}
	var followerScreen Rect
	if masterShowCmd == ShowNormal && current.ShowCmd == ShowNormal {
		followerScreen, err = GetWindowRect(hwnd)
		if err != nil {
			return false, err
		}
	}
	action, rect, placement := planWindowMirror(current, followerScreen, masterShowCmd, masterScreen, masterPlacement)
	switch action {
	case mirrorSetRect:
		err = SetWindowRect(hwnd, rect)
	case mirrorSetPlacement:
		err = SetPlacement(hwnd, placement)
	default:
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// arrangeAction is the planned way to place one window onto an
// arrange target rectangle (SCREEN coordinates).
type arrangeAction int

const (
	arrangeNoop arrangeAction = iota
	// arrangeSetRect applies the target through SetWindowRect.
	arrangeSetRect
	// arrangeRestoreThenSetRect first restores the window to
	// the normal state through SetWindowPlacement using the
	// window's OWN placement, then applies the target through
	// SetWindowRect.
	arrangeRestoreThenSetRect
)

// planArrange decides how a window reaches the arrange target,
// which is a SCREEN rectangle (a monitor work area in virtual
// screen coordinates). A normal-state window is already where
// the target says only if its actual SCREEN rectangle matches —
// its placement rectangle is workspace coordinates and is never
// compared against the target. A maximized or minimized window
// must change state first, whatever its stored rectangle says.
func planArrange(current Placement, currentScreen Rect, target Rect) arrangeAction {
	if current.ShowCmd != ShowNormal {
		return arrangeRestoreThenSetRect
	}
	if currentScreen == target {
		return arrangeNoop
	}
	return arrangeSetRect
}

// PlaceWindow moves a window to a rectangle in the normal show
// state — the arrange operation. target is a SCREEN rectangle
// (a monitor work area in virtual screen coordinates).
//
//   - A window already in the normal state is moved through
//     the geometry-only SetWindowRect path (SetWindowPos with
//     SWP_NOACTIVATE: no activation, no Z-order change,
//     foreground untouched).
//   - A maximized or minimized window is first restored to the
//     normal state through SetWindowPlacement using the
//     window's OWN placement — its stored workspace coordinates
//     are preserved verbatim and the screen target is NEVER
//     written into a WINDOWPLACEMENT — and the target is then
//     applied through SetWindowRect in screen coordinates. The
//     restore step is a show-state transition and carries the
//     documented limitation: no absolute foreground guarantee
//     is claimed for it.
func PlaceWindow(hwnd uintptr, target Rect) error {
	current, err := GetPlacement(hwnd)
	if err != nil {
		return err
	}
	var screen Rect
	if current.ShowCmd == ShowNormal {
		screen, err = GetWindowRect(hwnd)
		if err != nil {
			return err
		}
	}
	switch planArrange(current, screen, target) {
	case arrangeNoop:
		return nil
	case arrangeSetRect:
		return SetWindowRect(hwnd, target)
	default: // arrangeRestoreThenSetRect
		placement := current
		placement.ShowCmd = ShowNormal
		if err := SetPlacement(hwnd, placement); err != nil {
			return err
		}
		return SetWindowRect(hwnd, target)
	}
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
