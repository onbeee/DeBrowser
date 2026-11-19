//go:build !windows

package winmgr

// Window management is implemented for Windows only; other
// platforms report ErrUnsupported so callers can surface the
// boundary instead of failing obscurely.

// Monitors lists the displays' work areas.
func Monitors() ([]Monitor, error) { return nil, ErrUnsupported }

// WindowForPID finds the top-level window owned by a process.
func WindowForPID(pid int) (uintptr, error) { return 0, ErrUnsupported }

// GetPlacement reads a window's current placement.
func GetPlacement(hwnd uintptr) (Placement, error) { return Placement{}, ErrUnsupported }

// SetPlacement applies a placement without activating the window.
func SetPlacement(hwnd uintptr, p Placement) error { return ErrUnsupported }
