// Package uia is the narrow UI Automation surface
// synchronization uses to understand the browser's NATIVE UI:
// identifying the focused element (e.g. the address bar) so
// hook observations can be classified. It is never used to
// read page content, and elements that cannot be uniquely
// identified are treated as unknown by callers.
package uia

import "errors"

// ErrUnsupported marks UIA on non-Windows platforms or when
// the automation server cannot be reached.
var ErrUnsupported = errors.New("当前环境无法使用界面自动化识别")

// FocusInfo describes the focused UI element of a window.
type FocusInfo struct {
	Name        string
	ControlType int32 // UIA ControlType id (Edit=50004, Document=50030, ...)
}

// ControlType ids used by the classifier.
const (
	ControlTypeEdit     = 50004
	ControlTypeDocument = 50030
)

// FocusedElement returns the element that has keyboard focus
// within the given top-level window's process, or
// ErrUnsupported. It runs COM work on a dedicated thread and
// gives up on timeout rather than blocking the caller.
func FocusedElement(hwnd uintptr) (FocusInfo, error) { return focusedElement(hwnd) }

// LooksLikeAddressBar reports whether a focused element is
// plausibly the browser address bar: an Edit control whose
// automation name matches the well-known omnibox names of
// Chromium-family browsers. Deliberately conservative: any
// doubt returns false.
func LooksLikeAddressBar(f FocusInfo) bool {
	if f.ControlType != ControlTypeEdit {
		return false
	}
	switch f.Name {
	case "Address and search bar", "地址和搜索栏", "Address bar", "地址栏":
		return true
	}
	return false
}
