package service

import (
	"testing"

	"github.com/onbeee/DeBrowser/internal/wininput"
)

var (
	matchMainMaster   = wininput.WindowInfo{HWND: 100, Class: "Chrome_WidgetWin_1", Rect: wininput.Rect{X: 0, Y: 0, Width: 1200, Height: 800}}
	matchMainFollower = wininput.WindowInfo{HWND: 200, Class: "Chrome_WidgetWin_1", Rect: wininput.Rect{X: 1220, Y: 0, Width: 1200, Height: 800}}
)

func TestMatchMainWindowToMainWindow(t *testing.T) {
	target, from, ok := matchFollowerWindow(matchMainMaster, matchMainMaster, nil, matchMainFollower)
	if !ok || target.HWND != 200 || from != matchMainMaster.Rect {
		t.Fatalf("main→main = %+v, %v, %v", target, from, ok)
	}
}

func TestMatchPopupByClassAndOwnership(t *testing.T) {
	popupMaster := wininput.WindowInfo{
		HWND: 101, Class: "Chrome_WidgetWin_1", Owner: 100, Visible: true,
		Rect: wininput.Rect{X: 900, Y: 60, Width: 300, Height: 400},
	}
	followerPopup := wininput.WindowInfo{
		HWND: 201, Class: "Chrome_WidgetWin_1", Owner: 200, Visible: true,
		Rect: wininput.Rect{X: 2120, Y: 60, Width: 300, Height: 400},
	}
	target, from, ok := matchFollowerWindow(popupMaster, matchMainMaster,
		[]wininput.WindowInfo{matchMainFollower, followerPopup}, matchMainFollower)
	if !ok || target.HWND != 201 {
		t.Fatalf("popup match = %+v, %v — want the follower popup", target, ok)
	}
	if from != popupMaster.Rect {
		t.Fatalf("mapping source = %+v, want the master popup rect", from)
	}
}

func TestMatchPopupSkipsWhenNoCounterpart(t *testing.T) {
	popupMaster := wininput.WindowInfo{
		HWND: 101, Class: "Chrome_WidgetWin_1", Owner: 100, Visible: true,
	}
	// The follower has no popup: the event must be skipped, NOT
	// delivered to its main window.
	if _, _, ok := matchFollowerWindow(popupMaster, matchMainMaster,
		[]wininput.WindowInfo{matchMainFollower}, matchMainFollower); ok {
		t.Fatal("popup with no follower counterpart must not match the main window")
	}
}

func TestMatchPopupSkipsWhenAmbiguousOrMismatched(t *testing.T) {
	popupMaster := wininput.WindowInfo{
		HWND: 101, Class: "Chrome_WidgetWin_1", Owner: 100, Visible: true,
	}
	cand := func(hwnd uintptr, class string, owner uintptr) wininput.WindowInfo {
		return wininput.WindowInfo{HWND: hwnd, Class: class, Owner: owner, Visible: true}
	}
	// Two identical candidates: ambiguous.
	if _, _, ok := matchFollowerWindow(popupMaster, matchMainMaster,
		[]wininput.WindowInfo{matchMainFollower, cand(201, "Chrome_WidgetWin_1", 200), cand(202, "Chrome_WidgetWin_1", 200)},
		matchMainFollower); ok {
		t.Fatal("two candidates must not match")
	}
	// Class mismatch.
	if _, _, ok := matchFollowerWindow(popupMaster, matchMainMaster,
		[]wininput.WindowInfo{matchMainFollower, cand(201, "Other_Class", 200)},
		matchMainFollower); ok {
		t.Fatal("class mismatch must not match")
	}
	// Ownership mismatch: master's popup is owned by its main
	// window, the follower candidate is unowned.
	if _, _, ok := matchFollowerWindow(popupMaster, matchMainMaster,
		[]wininput.WindowInfo{matchMainFollower, cand(201, "Chrome_WidgetWin_1", 0)},
		matchMainFollower); ok {
		t.Fatal("ownership mismatch must not match")
	}
	// Hidden candidate does not count.
	hidden := cand(201, "Chrome_WidgetWin_1", 200)
	hidden.Visible = false
	if _, _, ok := matchFollowerWindow(popupMaster, matchMainMaster,
		[]wininput.WindowInfo{matchMainFollower, hidden}, matchMainFollower); ok {
		t.Fatal("hidden candidate must not match")
	}
}
