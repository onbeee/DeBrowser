//go:build !windows

package uia

func focusedElement(uintptr) (FocusInfo, error) { return FocusInfo{}, ErrUnsupported }
