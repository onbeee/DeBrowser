//go:build !windows

package winmgr

// Window management is implemented for Windows only; other
// platforms report ErrUnsupported so callers can surface the
// boundary instead of failing obscurely.

// Monitors lists the displays' work areas.
func Monitors() ([]Monitor, error) { return nil, ErrUnsupported }

// WindowForPID finds the top-level window owned by a process.
func WindowForPID(pid int) (uintptr, error) { return 0, ErrUnsupported }

// ForegroundWindow returns the current foreground window.
func ForegroundWindow() uintptr { return 0 }

// GetWindowRect reads a window's actual screen rectangle.
func GetWindowRect(hwnd uintptr) (Rect, error) { return Rect{}, ErrUnsupported }

// GetPlacement reads a window's full placement.
func GetPlacement(hwnd uintptr) (Placement, error) { return Placement{}, ErrUnsupported }

// SetPlacement applies a full placement (show state included).
func SetPlacement(hwnd uintptr, p Placement) error { return ErrUnsupported }

// SetWindowRect applies a geometry-only move/resize through
// the non-activating SetWindowPos path.
func SetWindowRect(hwnd uintptr, r Rect) error { return ErrUnsupported }
