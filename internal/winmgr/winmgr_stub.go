//go:build !windows

package winmgr

func ListMonitors() ([]Monitor, error) { return nil, ErrUnsupported }

func TopWindowForPID(pid uint32) (WindowInfo, bool) { return WindowInfo{}, false }

func ListWindowsForPID(pid uint32) []WindowInfo { return nil }

func WindowRect(hwnd uintptr) (Rect, bool) { return Rect{}, false }

func WindowStateOf(hwnd uintptr) WindowState { return StateNormal }

func SetWindowRect(hwnd uintptr, rect Rect) error { return ErrUnsupported }

func SetWindowState(hwnd uintptr, state WindowState) error { return ErrUnsupported }

func ForegroundWindow() uintptr { return 0 }

func WindowPID(hwnd uintptr) uint32 { return 0 }

func ClassifyPoint(x, y int32, pid uint32) Hit { return Hit{} }
