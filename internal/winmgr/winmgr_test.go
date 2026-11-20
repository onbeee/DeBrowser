package winmgr

import "testing"

func TestArrangeRectsTiles(t *testing.T) {
	work := Rect{X: 0, Y: 0, Width: 1920, Height: 1040}
	one := ArrangeRects(work, 1)
	if len(one) != 1 || one[0] != work {
		t.Fatalf("one window = %v", one)
	}
	four := ArrangeRects(work, 4)
	if len(four) != 4 || four[1].X != 960 || four[2].Y != 520 {
		t.Fatalf("four windows = %v", four)
	}
	// Negative-coordinate monitor keeps its origin.
	left := ArrangeRects(Rect{X: -1920, Y: 0, Width: 1920, Height: 1080}, 2)
	if left[0].X != -1920 || left[1].X != -960 {
		t.Fatalf("negative monitor = %v", left)
	}
	if got := ArrangeRects(work, 0); got != nil {
		t.Fatalf("zero windows = %v", got)
	}
}

func TestPlanWindowMirrorKeepsCoordinateSystemsApart(t *testing.T) {
	workspace := Rect{X: 8, Y: 8, Width: 900, Height: 600}  // placement coords
	screen := Rect{X: -1912, Y: 8, Width: 900, Height: 600} // actual screen rect
	master := Placement{ShowCmd: ShowNormal, NormalRect: workspace}
	follower := Placement{ShowCmd: ShowNormal}

	scr, pl := PlanWindowMirror(master, screen, follower)
	if pl != nil || scr == nil {
		t.Fatalf("normal/normal should use geometry path: scr=%v pl=%v", scr, pl)
	}
	if *scr != screen {
		t.Fatalf("mirrored rect = %v, want the SCREEN rect %v (not workspace %v)", *scr, screen, workspace)
	}

	// State difference → full placement path, payload untouched.
	master.ShowCmd = ShowMaximized
	scr, pl = PlanWindowMirror(master, screen, follower)
	if scr != nil || pl == nil || *pl != master {
		t.Fatalf("maximized should use placement path verbatim: scr=%v pl=%v", scr, pl)
	}

	// Minimized master is never mirrored.
	master.ShowCmd = ShowMinimized
	scr, pl = PlanWindowMirror(master, screen, follower)
	if scr != nil || pl != nil {
		t.Fatalf("minimized master must be a no-op: scr=%v pl=%v", scr, pl)
	}
}

func TestPlanArrangeRestoresWithOwnPlacement(t *testing.T) {
	target := Rect{X: 960, Y: 0, Width: 960, Height: 1040}
	maximized := Placement{ShowCmd: ShowMaximized, NormalRect: Rect{X: 100, Y: 100, Width: 800, Height: 500}}
	restore, scr := PlanArrange(maximized, Rect{}, target)
	if restore == nil || scr == nil {
		t.Fatalf("maximized arrange needs restore+geometry: %v %v", restore, scr)
	}
	if restore.NormalRect != maximized.NormalRect || restore.ShowCmd != ShowNormal {
		t.Fatalf("restore must carry the window's OWN placement: %+v", restore)
	}
	if *scr != target {
		t.Fatalf("geometry target = %v, want %v", *scr, target)
	}

	normal := Placement{ShowCmd: ShowNormal}
	restore, scr = PlanArrange(normal, Rect{X: 0, Y: 0, Width: 100, Height: 100}, target)
	if restore != nil || scr == nil || *scr != target {
		t.Fatalf("normal arrange is pure geometry: %v %v", restore, scr)
	}
	restore, scr = PlanArrange(normal, target, target)
	if restore != nil || scr != nil {
		t.Fatalf("already in place is a no-op: %v %v", restore, scr)
	}
}

func TestPlacementCarriesFullWindowState(t *testing.T) {
	a := Placement{NormalRect: Rect{1, 2, 3, 4}, MinPosition: Point{-1, -1}, MaxPosition: Point{5, 6}, ShowCmd: ShowMaximized, Flags: 2}
	b := a
	if !PlacementsEqual(a, b) {
		t.Fatal("identical placements must compare equal")
	}
	b.ShowCmd = ShowNormal
	if PlacementsEqual(a, b) {
		t.Fatal("different ShowCmd must compare unequal")
	}
}

func TestPickMainWindow(t *testing.T) {
	chrome := WindowInfo{HWND: 11, ClassName: "Chrome_WidgetWin_1"}
	other := WindowInfo{HWND: 22, ClassName: "SomeOtherClass"}
	chrome2 := WindowInfo{HWND: 33, ClassName: "Chrome_WidgetWin_1"}

	if hwnd, err := PickMainWindow([]WindowInfo{other, chrome}); err != nil || hwnd != 11 {
		t.Fatalf("one chromium window among others = %d, %v", hwnd, err)
	}
	if hwnd, err := PickMainWindow([]WindowInfo{other}); err != nil || hwnd != 22 {
		t.Fatalf("single candidate = %d, %v", hwnd, err)
	}
	if _, err := PickMainWindow([]WindowInfo{chrome, chrome2}); err == nil {
		t.Fatal("two chromium windows are ambiguous and must be rejected")
	}
	if _, err := PickMainWindow([]WindowInfo{other, {HWND: 44, ClassName: "Another"}}); err == nil {
		t.Fatal("multiple non-chromium candidates are ambiguous")
	}
	if _, err := PickMainWindow(nil); err == nil {
		t.Fatal("no windows must error")
	}
}
