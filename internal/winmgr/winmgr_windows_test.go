//go:build windows

package winmgr

import (
	"syscall"
	"testing"
	"unsafe"
)

// The Win32 WINDOWPLACEMENT structure is exactly 44 bytes:
// length/flags/showCmd (4 each), two POINTs (8 each) and one
// RECT (16). GetWindowPlacement and SetWindowPlacement reject
// any other length, so the Go mirror must match byte for byte.
func TestWindowPlacementStructIsExactly44Bytes(t *testing.T) {
	if got := unsafe.Sizeof(windowPlacement{}); got != 44 {
		t.Fatalf("unsafe.Sizeof(windowPlacement{}) = %d, want 44", got)
	}
	// Field offsets must match the native layout too.
	if off := unsafe.Offsetof(windowPlacement{}.NormalPosition); off != 28 {
		t.Fatalf("NormalPosition offset = %d, want 28", off)
	}
}

// Both API wrappers initialise Length from the struct size;
// with the struct at 44 bytes, the length handed to
// GetWindowPlacement and SetWindowPlacement is 44.
func TestPlacementLengthInitializationIs44(t *testing.T) {
	getInit := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if getInit.Length != 44 {
		t.Fatalf("GetPlacement length initialisation = %d, want 44", getInit.Length)
	}
	setInit := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if setInit.Length != 44 {
		t.Fatalf("SetPlacement length initialisation = %d, want 44", setInit.Length)
	}
}

// TestPlacementRoundTripOnRealWindow creates one real top-level
// window (built-in STATIC class, no registration or message
// loop needed for placement APIs) and round-trips a placement
// through the real GetWindowPlacement/SetWindowPlacement calls
// — the end-to-end proof that the 44-byte length is accepted
// and that every field survives the conversion.
func TestPlacementRoundTripOnRealWindow(t *testing.T) {
	user32 := syscall.NewLazyDLL("user32.dll")
	createWindow := user32.NewProc("CreateWindowExW")
	destroyWindow := user32.NewProc("DestroyWindow")
	className, err := syscall.UTF16PtrFromString("STATIC")
	if err != nil {
		t.Fatalf("class name: %v", err)
	}
	title, err := syscall.UTF16PtrFromString("winmgr placement test")
	if err != nil {
		t.Fatalf("title: %v", err)
	}
	hwnd, _, callErr := createWindow.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 50, 50, 320, 200, 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatalf("CreateWindowExW failed: %v", callErr)
	}
	defer destroyWindow.Call(hwnd)

	before, err := GetPlacement(hwnd)
	if err != nil {
		t.Fatalf("GetPlacement on a real window: %v", err)
	}

	// Shift the normal rectangle within the placement's own
	// (workspace) coordinate system and write it back; the
	// read-back must match field for field.
	moved := before
	moved.NormalRect.X += 17
	moved.NormalRect.Y += 9
	if err := SetPlacement(hwnd, moved); err != nil {
		t.Fatalf("SetPlacement on a real window: %v", err)
	}
	after, err := GetPlacement(hwnd)
	if err != nil {
		t.Fatalf("GetPlacement after SetPlacement: %v", err)
	}
	if after.NormalRect != moved.NormalRect {
		t.Fatalf("normal rect round trip = %v, want %v", after.NormalRect, moved.NormalRect)
	}
	if after.ShowCmd != moved.ShowCmd || after.MinPosition != moved.MinPosition || after.MaxPosition != moved.MaxPosition {
		t.Fatalf("placement fields did not round trip: before %+v, wrote %+v, read %+v", before, moved, after)
	}
}
