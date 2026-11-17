//go:build !windows

package wininput

// Non-Windows stubs: every native capability reports
// ErrUnsupported so callers degrade honestly (sync cannot start)
// instead of pretending. DeBrowser ships for Windows; these exist
// for cross-platform builds and tests.

func StartCapture(func(Event)) error { return ErrUnsupported }
func StopCapture()                   {}
func HitTest(x, y int32) (Hit, bool) { return Hit{}, false }
func TopWindowForPID(pid uint32) (uintptr, bool) {
	return 0, false
}
func OwnerWindow(hwnd uintptr) uintptr { return 0 }
func WindowPID(hwnd uintptr) uint32    { return 0 }
func GetWindowRect(hwnd uintptr) (Rect, bool) {
	return Rect{}, false
}
func ForegroundWindow() uintptr { return 0 }
func ActivateWindow(hwnd uintptr) bool {
	return false
}
func InjectMouse(ev Event)        {}
func InjectKey(vk int, down bool) {}
func ListMonitors() []Monitor     { return nil }
func MoveWindow(hwnd uintptr, r Rect) error {
	return ErrUnsupported
}
func WindowPlacement(hwnd uintptr) (Rect, bool) {
	return Rect{}, false
}
func IsMinimized(hwnd uintptr) bool { return false }
