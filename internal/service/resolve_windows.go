//go:build windows

package service

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// resolveFinalPath returns the fully resolved path of p with EVERY
// reparse point (symbolic links, junctions, mount points) expanded,
// using GetFinalPathNameByHandle — the canonical Windows API for
// this. filepath.EvalSymlinks is not sufficient here: its component
// walk does not reliably detect a junction that is the FINAL path
// component (verified empirically: a junction used as a profile dir
// itself slipped through EvalSymlinks-based checks), while the
// final-path API always resolves it.
func resolveFinalPath(p string) (string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	// FILE_FLAG_BACKUP_SEMANTICS (0x02000000) is required to open a
	// directory handle.
	const fileFlagBackupSemantics = 0x02000000
	h, err := syscall.CreateFile(
		pathPtr,
		0, // no access requested: a pure path-resolution handle
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		fileFlagBackupSemantics,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open for final path: %w", err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	getFinalPath := syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		r1, _, callErr := getFinalPath.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			0, // FILE_NAME_NORMALIZED | VOLUME_NAME_DOS
		)
		if r1 == 0 {
			return "", fmt.Errorf("GetFinalPathNameByHandle: %w", callErr)
		}
		if int(r1) < len(buf) {
			out := syscall.UTF16ToString(buf[:r1])
			// Normalize the \?\ extended-length form back to a
			// regular DOS path so containment comparisons are
			// apples-to-apples (both sides go through this func).
			out = strings.TrimPrefix(out, `\\?\`)
			if strings.HasPrefix(out, `UNC\`) {
				out = `\` + strings.TrimPrefix(out, `UNC`)
			}
			return out, nil
		}
		buf = make([]uint16, int(r1)+1)
	}
}
