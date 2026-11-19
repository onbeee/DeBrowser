package capture

import "testing"

func TestClassifyZones(t *testing.T) {
	owner := func(hwnd uintptr) int {
		if hwnd == 100 {
			return 42 // master window
		}
		return 7 // someone else's window
	}
	ev := RawHookEvent{HWND: 100, IsKey: true}
	if z := Classify(ev, 42, owner, false); z != ZoneNativeUI {
		t.Fatalf("master window without page echo = %v, want NativeUI", z)
	}
	if z := Classify(ev, 42, owner, true); z != ZonePage {
		t.Fatalf("master window with page echo = %v, want Page", z)
	}
	ev.HWND = 200
	if z := Classify(ev, 42, owner, false); z != ZoneUnknown {
		t.Fatalf("foreign window = %v, want Unknown", z)
	}
	ev.HWND = 0
	if z := Classify(ev, 42, owner, false); z != ZoneUnknown {
		t.Fatalf("no window = %v, want Unknown", z)
	}
	if z := Classify(RawHookEvent{HWND: 100}, 0, owner, false); z != ZoneUnknown {
		t.Fatalf("no master = %v, want Unknown", z)
	}
}
