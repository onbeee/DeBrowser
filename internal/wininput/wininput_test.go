package wininput

import "testing"

func TestMapPoint(t *testing.T) {
	from := Rect{X: 100, Y: 100, Width: 1000, Height: 500}
	to := Rect{X: 0, Y: 0, Width: 500, Height: 250}
	x, y := MapPoint(600, 350, from, to)
	if x != 250 || y != 125 {
		t.Fatalf("mapped = %d,%d want 250,125", x, y)
	}
	x, y = MapPoint(100, 100, from, to)
	if x != 0 || y != 0 {
		t.Fatalf("origin mapped = %d,%d want 0,0", x, y)
	}
}

func TestGridRects(t *testing.T) {
	work := Rect{X: 0, Y: 0, Width: 1920, Height: 1040}
	for _, n := range []int{1, 2, 3, 5, 8} {
		rects := GridRects(work, n)
		if len(rects) != n {
			t.Fatalf("n=%d: got %d rects", n, len(rects))
		}
		for i, r := range rects {
			if r.Empty() {
				t.Fatalf("n=%d rect %d empty", n, i)
			}
			if r.X < work.X || r.Y < work.Y ||
				r.X+r.Width > work.X+work.Width || r.Y+r.Height > work.Y+work.Height {
				t.Fatalf("n=%d rect %d %+v escapes work area", n, i, r)
			}
		}
	}
	if got := GridRects(work, 0); got != nil {
		t.Fatalf("n=0: got %v", got)
	}
	// 5 windows: 3 columns x 2 rows; the master is first.
	rects := GridRects(work, 5)
	if rects[0].X != 0 || rects[0].Y != 0 {
		t.Fatalf("master rect = %+v, want at origin", rects[0])
	}
}
