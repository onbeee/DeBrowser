package service

import (
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// RestoreWindows record retention: records are consumed per
// window — a successful restore removes only its own record, a
// failed restore keeps the original placement for a later
// retry, one failure never stops the other windows, and a
// retry can finish the job.

func TestRestoreWindowsRetainsOnlyFailedRecordsAndRetryCompletes(t *testing.T) {
	plA := winmgr.Placement{NormalRect: winmgr.Rect{X: 0, Y: 0, Width: 800, Height: 600}, ShowCmd: winmgr.ShowNormal}
	plB := winmgr.Placement{NormalRect: winmgr.Rect{X: -1600, Y: 0, Width: 800, Height: 600}, ShowCmd: winmgr.ShowMaximized}
	plC := winmgr.Placement{NormalRect: winmgr.Rect{X: 100, Y: 100, Width: 640, Height: 480}, ShowCmd: winmgr.ShowNormal}
	svc := &SyncService{savedPlacements: map[string]savedPlacement{
		"a": {hwnd: 101, placement: plA},
		"b": {hwnd: 102, placement: plB},
		"c": {hwnd: 103, placement: plC},
	}}

	attempts := map[uintptr]int{}
	orig := setWindowPlacement
	setWindowPlacement = func(hwnd uintptr, p winmgr.Placement) error {
		attempts[hwnd]++
		if hwnd == 102 {
			return errors.New("fake restore failure")
		}
		return nil
	}
	t.Cleanup(func() { setWindowPlacement = orig })

	if err := svc.RestoreWindows(""); err == nil {
		t.Fatal("a partial restore failure must be reported")
	}
	// Every window was attempted exactly once: b's failure did
	// not stop a or c.
	for _, hwnd := range []uintptr{101, 102, 103} {
		if attempts[hwnd] != 1 {
			t.Fatalf("window %d attempted %d times, want 1", hwnd, attempts[hwnd])
		}
	}
	// Only the failed window's record remains, with its
	// ORIGINAL placement untouched.
	svc.mu.Lock()
	remaining := make(map[string]savedPlacement, len(svc.savedPlacements))
	for id, sp := range svc.savedPlacements {
		remaining[id] = sp
	}
	svc.mu.Unlock()
	if len(remaining) != 1 {
		t.Fatalf("remaining records = %v, want only the failed window", remaining)
	}
	sp, ok := remaining["b"]
	if !ok || sp.hwnd != 102 || sp.placement != plB {
		t.Fatalf("retained record = %+v ok=%v, want b with its original placement", sp, ok)
	}

	// Retry with the failure gone: the retained record restores
	// and the records are fully cleared.
	setWindowPlacement = func(hwnd uintptr, p winmgr.Placement) error {
		attempts[hwnd]++
		if p != plB {
			t.Errorf("retry applied placement %+v, want the retained original %+v", p, plB)
		}
		return nil
	}
	if err := svc.RestoreWindows(""); err != nil {
		t.Fatalf("retry restore: %v", err)
	}
	if attempts[102] != 2 || attempts[101] != 1 || attempts[103] != 1 {
		t.Fatalf("attempts after retry = %v, want only b retried", attempts)
	}
	svc.mu.Lock()
	left := len(svc.savedPlacements)
	svc.mu.Unlock()
	if left != 0 {
		t.Fatalf("records after full success = %d, want 0", left)
	}

	// With nothing retained, restoring again is the documented
	// "nothing to restore" error, not a silent success.
	if err := svc.RestoreWindows(""); err == nil {
		t.Fatal("restore with no records must report that there is nothing to restore")
	}
}
