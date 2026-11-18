//go:build !windows

package uia

func ProbeWindow(hwnd uintptr) (Capabilities, error) {
	return Capabilities{SelectedTab: -1, Reason: "当前平台不支持 UI Automation"}, ErrUnsupported
}

func SelectedTabIndex(hwnd uintptr) (int, bool) { return -1, false }

func SelectTab(hwnd uintptr, index int) error { return ErrUnsupported }

func InvokeButton(hwnd uintptr, spec MatchSpec) error { return ErrUnsupported }
