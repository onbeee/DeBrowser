package winmgr

import "testing"

func TestGridRectsTiles(t *testing.T) {
	work := Rect{X: 0, Y: 0, Width: 1920, Height: 1080}
	if got := GridRects(work, 1); len(got) != 1 || got[0] != work {
		t.Fatalf("single tile = %v, want the whole work area", got)
	}
	got := GridRects(work, 4)
	if len(got) != 4 {
		t.Fatalf("tiles = %d, want 4", len(got))
	}
	if got[0] != (Rect{X: 0, Y: 0, Width: 960, Height: 540}) ||
		got[3] != (Rect{X: 960, Y: 540, Width: 960, Height: 540}) {
		t.Fatalf("2x2 tiles = %v", got)
	}
	// Tiles cover the area without gaps or overlap.
	total := int64(0)
	for _, r := range got {
		total += int64(r.Width) * int64(r.Height)
	}
	if total != int64(work.Width)*int64(work.Height) {
		t.Fatalf("tile area = %d, want %d", total, int64(work.Width)*int64(work.Height))
	}
}

func TestGridRectsNegativeCoordinates(t *testing.T) {
	// A monitor left of the primary lives at negative X.
	work := Rect{X: -1600, Y: 0, Width: 1600, Height: 900}
	got := GridRects(work, 2)
	if len(got) != 2 || got[0].X != -1600 || got[1].X != -800 {
		t.Fatalf("negative-origin tiles = %v", got)
	}
}

func TestGridRectsEmpty(t *testing.T) {
	if got := GridRects(Rect{Width: 100, Height: 100}, 0); got != nil {
		t.Fatalf("zero windows = %v, want nil", got)
	}
}

func TestPlacementsEqualComparesShowCmdAndNormalRect(t *testing.T) {
	base := Placement{
		NormalRect:  Rect{X: -1600, Y: 10, Width: 800, Height: 600}, // negative-coordinate monitor
		MinPosition: Point{X: -1600, Y: 0},
		MaxPosition: Point{X: 0, Y: 0},
		ShowCmd:     ShowNormal,
	}
	if !PlacementsEqual(base, base) {
		t.Fatal("identical placements must compare equal")
	}
	moved := base
	moved.NormalRect.X += 10
	if PlacementsEqual(base, moved) {
		t.Fatal("a moved normal rectangle must compare unequal")
	}
	maxed := base
	maxed.ShowCmd = ShowMaximized
	if PlacementsEqual(base, maxed) {
		t.Fatal("a different show command must compare unequal")
	}
	// Min/max positions alone do not constitute a visible change.
	shifted := base
	shifted.MinPosition = Point{X: 5, Y: 5}
	if !PlacementsEqual(base, shifted) {
		t.Fatal("min position alone should not trigger mirroring")
	}
}

func TestMirrorPlacementNeverMinimizesAFollower(t *testing.T) {
	// The minimize boundary short-circuits before any platform
	// call, so this holds on every platform: mirroring a
	// minimized master must not hide a follower window.
	applied, err := MirrorPlacement(0, ShowMinimized, Rect{}, Placement{ShowCmd: ShowMinimized})
	if err != nil {
		t.Fatalf("minimize mirror returned error: %v", err)
	}
	if applied {
		t.Fatal("minimize must never be mirrored")
	}
}

func TestPlacementCarriesFullWindowState(t *testing.T) {
	// A placement is the full WINDOWPLACEMENT state — normal
	// rect, min/max positions and show command — not a rect plus
	// two booleans. This guards the struct contract the Windows
	// implementation round-trips.
	p := Placement{
		NormalRect:  Rect{X: 100, Y: 50, Width: 1024, Height: 768},
		MinPosition: Point{X: -32000, Y: -32000},
		MaxPosition: Point{X: 0, Y: 0},
		ShowCmd:     ShowMaximized,
		Flags:       0,
	}
	if p.NormalRect.Width != 1024 || p.ShowCmd != ShowMaximized || p.MinPosition.X != -32000 {
		t.Fatalf("placement fields not preserved: %+v", p)
	}
	if ShowNormal != 1 || ShowMinimized != 2 || ShowMaximized != 3 {
		t.Fatal("show command values must mirror SW_SHOWNORMAL/SW_SHOWMINIMIZED/SW_SHOWMAXIMIZED")
	}
}

func TestPlanWindowMirrorNormalGeometryUsesMasterScreenRectVerbatim(t *testing.T) {
	// The follower's placement rectangle is WORKSPACE
	// coordinates (work area origin at 0,0); its actual SCREEN
	// rectangle is offset by the taskbar height (40). The
	// master sits on a monitor at negative screen X. Mirroring
	// must plan the master's SCREEN rectangle verbatim —
	// negative coordinates included — never its placement
	// rectangle.
	follower := Placement{NormalRect: Rect{X: 0, Y: 0, Width: 800, Height: 600}, ShowCmd: ShowNormal}
	followerScreen := Rect{X: 0, Y: 40, Width: 800, Height: 600}
	masterPlacement := Placement{NormalRect: Rect{X: -1600, Y: 0, Width: 800, Height: 600}, ShowCmd: ShowNormal}
	masterScreen := Rect{X: -1600, Y: 40, Width: 800, Height: 600}

	action, rect, _ := planWindowMirror(follower, followerScreen, ShowNormal, masterScreen, masterPlacement)
	if action != mirrorSetRect {
		t.Fatalf("action = %v, want mirrorSetRect", action)
	}
	if rect != masterScreen {
		t.Fatalf("planned rect = %v, want the master screen rect %v verbatim", rect, masterScreen)
	}
	if rect == masterPlacement.NormalRect {
		t.Fatal("planned rect equals the master's WORKSPACE rect: coordinate systems were mixed")
	}

	// Already at the master's screen rectangle: no action,
	// even though the workspace rectangles differ.
	action, _, _ = planWindowMirror(follower, masterScreen, ShowNormal, masterScreen, masterPlacement)
	if action != mirrorNoop {
		t.Fatalf("action = %v, want mirrorNoop when screen rects match", action)
	}
}

func TestPlanWindowMirrorStateTransitionUsesPlacementVerbatim(t *testing.T) {
	// Master maximized, follower normal: a state transition.
	// The payload must be the master's PLACEMENT verbatim
	// (workspace coordinates stay inside the placement system);
	// no screen rectangle may be converted into it.
	follower := Placement{NormalRect: Rect{X: 0, Y: 0, Width: 800, Height: 600}, ShowCmd: ShowNormal}
	masterPlacement := Placement{
		NormalRect:  Rect{X: -1600, Y: 0, Width: 1024, Height: 768},
		MinPosition: Point{X: -32000, Y: -32000},
		MaxPosition: Point{X: -1600, Y: 0},
		ShowCmd:     ShowMaximized,
	}
	masterScreen := Rect{X: -1600, Y: 0, Width: 1600, Height: 900}
	action, _, placement := planWindowMirror(follower, Rect{X: 0, Y: 40, Width: 800, Height: 600}, ShowMaximized, masterScreen, masterPlacement)
	if action != mirrorSetPlacement {
		t.Fatalf("action = %v, want mirrorSetPlacement", action)
	}
	if placement != masterPlacement {
		t.Fatalf("planned placement = %+v, want the master placement %+v verbatim", placement, masterPlacement)
	}

	// Both maximized with identical placements: nothing to do.
	action, _, _ = planWindowMirror(masterPlacement, masterScreen, ShowMaximized, masterScreen, masterPlacement)
	if action != mirrorNoop {
		t.Fatalf("action = %v, want mirrorNoop for identical maximized placements", action)
	}

	// Master minimized: never mirrored.
	action, _, _ = planWindowMirror(follower, Rect{}, ShowMinimized, Rect{}, Placement{ShowCmd: ShowMinimized})
	if action != mirrorNoop {
		t.Fatalf("action = %v, want mirrorNoop for a minimized master", action)
	}
}

func TestPlanArrangeKeepsCoordinateSystemsApart(t *testing.T) {
	target := Rect{X: -1600, Y: 40, Width: 800, Height: 860} // screen coords: left monitor work area

	// Normal window already at the target SCREEN rect: noop,
	// even though its workspace NormalRect differs.
	normal := Placement{NormalRect: Rect{X: -1600, Y: 0, Width: 800, Height: 860}, ShowCmd: ShowNormal}
	if got := planArrange(normal, target, target); got != arrangeNoop {
		t.Fatalf("planArrange(normal, at target) = %v, want arrangeNoop", got)
	}
	// Normal window elsewhere on screen: geometry only.
	if got := planArrange(normal, Rect{X: 0, Y: 40, Width: 800, Height: 860}, target); got != arrangeSetRect {
		t.Fatalf("planArrange(normal, elsewhere) = %v, want arrangeSetRect", got)
	}
	// Maximized window: restore-then-set-rect — even when its
	// stored NormalRect happens to equal the target, the state
	// transition comes first and the target is applied as a
	// screen rectangle afterwards, never written into the
	// placement.
	maxed := Placement{NormalRect: target, ShowCmd: ShowMaximized}
	if got := planArrange(maxed, Rect{X: -1600, Y: 0, Width: 1600, Height: 900}, target); got != arrangeRestoreThenSetRect {
		t.Fatalf("planArrange(maximized) = %v, want arrangeRestoreThenSetRect", got)
	}
	minned := Placement{NormalRect: target, ShowCmd: ShowMinimized}
	if got := planArrange(minned, Rect{}, target); got != arrangeRestoreThenSetRect {
		t.Fatalf("planArrange(minimized) = %v, want arrangeRestoreThenSetRect", got)
	}
}
