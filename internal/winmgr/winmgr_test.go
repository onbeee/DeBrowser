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
