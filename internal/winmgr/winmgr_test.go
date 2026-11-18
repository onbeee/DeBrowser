package winmgr

import "testing"

func TestGridRects(t *testing.T) {
	area := Rect{X: 0, Y: 0, Width: 1920, Height: 1040}
	if got := GridRects(area, 0); got != nil {
		t.Fatalf("n=0: %v", got)
	}
	one := GridRects(area, 1)
	if len(one) != 1 || one[0] != area {
		t.Fatalf("n=1: %v", one)
	}
	five := GridRects(area, 5)
	if len(five) != 5 {
		t.Fatalf("n=5: %d cells", len(five))
	}
	for _, r := range five {
		if r.X < area.X || r.Y < area.Y || r.X+r.Width > area.X+area.Width || r.Y+r.Height > area.Y+area.Height {
			t.Fatalf("cell escapes work area: %+v", r)
		}
		if r.Width <= 0 || r.Height <= 0 {
			t.Fatalf("degenerate cell: %+v", r)
		}
	}
	// Last column/row absorb the remainder exactly.
	three := GridRects(Rect{X: 10, Y: 20, Width: 1000, Height: 700}, 3)
	if three[1].X+three[1].Width != 1010 {
		t.Fatalf("grid does not fill width exactly: %+v", three[1])
	}
	if three[2].Y+three[2].Height != 720 {
		t.Fatalf("grid does not fill height exactly: %+v", three[2])
	}
}

func TestMapPoint(t *testing.T) {
	from := Rect{X: 0, Y: 0, Width: 1000, Height: 800}
	to := Rect{X: 100, Y: 50, Width: 500, Height: 400}
	x, y := MapPoint(500, 400, from, to)
	if x != 350 || y != 250 {
		t.Fatalf("MapPoint center = (%d,%d), want (350,250)", x, y)
	}
	x, y = MapPoint(10, 10, Rect{}, to)
	if x != to.X || y != to.Y {
		t.Fatalf("degenerate from-rect = (%d,%d)", x, y)
	}
}
