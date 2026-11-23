//go:build !windows

package winmgr

func getPlacement(uintptr) (Placement, error)      { return Placement{}, ErrUnsupported }
func setPlacement(uintptr, Placement) error        { return ErrUnsupported }
func windowScreenRect(uintptr) (Rect, error)       { return Rect{}, ErrUnsupported }
func setWindowRect(uintptr, Rect) error            { return ErrUnsupported }
func windowOwnerPID(uintptr) int                   { return 0 }
func listMonitors() ([]Monitor, error)             { return nil, ErrUnsupported }
func listProcessWindows(int) ([]WindowInfo, error) { return nil, ErrUnsupported }
