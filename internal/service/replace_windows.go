//go:build windows

package service

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Windows replacement uses the native MoveFileExW with
// MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH: the
// destination is replaced atomically by the OS (never deleted
// first by us), and the call only reports success once the move
// is durable. If the call fails, an existing destination keeps its
// original bytes.
var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

const (
	movefileReplaceExisting = 0x00000001
	movefileWriteThrough    = 0x00000008
)

func platformReplaceFile(src, dst string) error {
	srcPtr, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return fmt.Errorf("替换目标路径无效：%w", err)
	}
	dstPtr, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return fmt.Errorf("替换目标路径无效：%w", err)
	}
	r, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(srcPtr)),
		uintptr(unsafe.Pointer(dstPtr)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if r == 0 {
		return fmt.Errorf("MoveFileExW 替换失败：%w", callErr)
	}
	return nil
}
