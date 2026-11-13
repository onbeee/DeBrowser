//go:build windows

package secrets

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Windows atomic file replacement via MoveFileExW with
// MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH.
//
// The Stage 4A review requirement is explicit: plain os.Rename
// must not be ASSUMED to replace an existing destination reliably
// on Windows. MoveFileExW with REPLACE_EXISTING is the documented
// primitive: within one volume the rename is atomic (readers see
// the old file or the new file, never a partial one), and
// WRITE_THROUGH makes the operation wait for the metadata to reach
// the storage device before reporting success. Temp file and target
// always live in the same directory, so this is always a
// same-volume rename.

var procMoveFileExW = kernel32ForSecrets.NewProc("MoveFileExW")

const (
	movefileReplaceExisting = 0x1
	movefileWriteThrough    = 0x8
)

func replaceFile(src, dst string) error {
	srcW, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return fmt.Errorf("secrets: encode source path: %w", err)
	}
	dstW, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return fmt.Errorf("secrets: encode destination path: %w", err)
	}
	r, _, callErr := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(srcW)),
		uintptr(unsafe.Pointer(dstW)),
		uintptr(movefileReplaceExisting|movefileWriteThrough),
	)
	if r == 0 {
		return fmt.Errorf("secrets: MoveFileExW replace failed: %v", callErr)
	}
	return nil
}

// syncDir is a no-op on Windows: MoveFileExW with
// MOVEFILE_WRITE_THROUGH already provides the durability ordering
// the vault protocol needs.
func syncDir(_ string) {}
