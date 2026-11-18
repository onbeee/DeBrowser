//go:build !windows

package uia

func ProbeWindow(hwnd uintptr) (Capabilities, error) {
	return Capabilities{SelectedTab: -1, Reason: "当前平台不支持 UI Automation"}, ErrUnsupported
}

func TabStrip(hwnd uintptr) (titles []string, selected int, ok bool) { return nil, -1, false }

func MenuButtonState(hwnd uintptr) (expanded bool, known bool) { return false, false }

func ExecuteUIOp(hwnd uintptr, op UIOp) error { return ErrUnsupported }
