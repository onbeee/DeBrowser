//go:build windows

package winmgr

import (
	"syscall"
	"testing"
	"unsafe"
)

// The native WINDOWPLACEMENT is exactly 44 bytes and both API
// wrappers initialise Length from the struct size.
func TestWindowPlacementStructIsExactly44Bytes(t *testing.T) {
	if got := unsafe.Sizeof(windowPlacement{}); got != 44 {
		t.Fatalf("unsafe.Sizeof(windowPlacement{}) = %d, want 44", got)
	}
	if off := unsafe.Offsetof(windowPlacement{}.NormalPosition); off != 28 {
		t.Fatalf("NormalPosition offset = %d, want 28", off)
	}
	wp := windowPlacement{Length: uint32(unsafe.Sizeof(windowPlacement{}))}
	if wp.Length != 44 {
		t.Fatalf("Length initialisation = %d, want 44", wp.Length)
	}
}

// TestPlacementRoundTripOnRealWindow round-trips a placement
// through the real Get/SetWindowPlacement on a real window.
func TestPlacementRoundTripOnRealWindow(t *testing.T) {
	user32 := syscall.NewLazyDLL("user32.dll")
	createWindow := user32.NewProc("CreateWindowExW")
	destroyWindow := user32.NewProc("DestroyWindow")
	className, _ := syscall.UTF16PtrFromString("STATIC")
	title, _ := syscall.UTF16PtrFromString("winmgr placement test")
	hwnd, _, callErr := createWindow.Call(0, uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)), 0, 50, 50, 320, 200, 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatalf("CreateWindowExW failed: %v", callErr)
	}
	defer destroyWindow.Call(hwnd)

	before, err := GetPlacement(hwnd)
	if err != nil {
		t.Fatalf("GetPlacement: %v", err)
	}
	moved := before
	moved.NormalRect.X += 13
	moved.NormalRect.Y += 7
	if err := SetPlacement(hwnd, moved); err != nil {
		t.Fatalf("SetPlacement: %v", err)
	}
	after, err := GetPlacement(hwnd)
	if err != nil {
		t.Fatalf("GetPlacement after set: %v", err)
	}
	if after.NormalRect != moved.NormalRect || after.ShowCmd != moved.ShowCmd {
		t.Fatalf("round trip mismatch: wrote %+v, read %+v", moved, after)
	}
	if got := WindowOwnerPID(hwnd); got <= 0 {
		t.Fatalf("WindowOwnerPID = %d, want a real pid", got)
	}
}
