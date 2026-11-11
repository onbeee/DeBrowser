//go:build windows

package safedelete

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// removeTree implements RemoveTree on Windows. Windows has no
// unlinkat-style handle-relative deletion in the standard library,
// so the anchoring is done by VERIFICATION PER DIRECTORY: before a
// directory's children are touched, the directory's own final path
// (GetFinalPathNameByHandle — resolves every reparse point) must
// equal its lexical path. Any entry that resolves elsewhere is a
// reparse point (symlink, junction, mount point) and is removed as
// a link object only: RemoveDirectory/DeleteFile on a reparse point
// removes the point itself, never its target's contents.
func removeTree(root, target string) error {
	return removeDir(target)
}

func removeDir(dir string) error {
	resolved, err := finalPath(dir)
	if err != nil {
		return fmt.Errorf("safedelete: resolve %s: %w", dir, err)
	}
	// A directory counts as a reparse point when resolving it lands
	// somewhere other than <resolved parent>\<its own name>. The
	// comparison must NOT be against the lexical path: the resolver
	// returns long-form names while callers may hold 8.3 short forms
	// (e.g. C:\Users\RUNNER~1\... from %TEMP%), which made plain
	// directories look like reparse points in CI.
	resolvedParent, err := finalPath(filepath.Dir(dir))
	if err != nil {
		return fmt.Errorf("safedelete: resolve parent of %s: %w", dir, err)
	}
	if !pathsEqual(resolved, filepath.Join(resolvedParent, filepath.Base(dir))) {
		// This directory itself is a reparse point: remove the
		// point only. Its target's contents are not touched.
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("safedelete: remove reparse point %s: %w", dir, err)
		}
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("safedelete: read dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			// IsDir follows the entry info from ReadDir; a junction
			// also reports as a directory, so removeDir's own
			// final-path verification decides: reparse points are
			// unlinked, real directories are emptied recursively.
			if err := removeDir(child); err != nil {
				return err
			}
			continue
		}
		// Files and non-directory links: os.Remove deletes the
		// entry itself (for a symlink, the link), never the target.
		if err := os.Remove(child); err != nil {
			return fmt.Errorf("safedelete: remove %s: %w", child, err)
		}
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("safedelete: rmdir %s: %w", dir, err)
	}
	return nil
}

// pathsEqual compares two Windows paths case-insensitively (the
// filesystem is case-insensitive) after cleaning.
func pathsEqual(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// finalPath resolves every reparse point in p via
// GetFinalPathNameByHandle (see the service package for why
// filepath.EvalSymlinks alone is not reliable for junctions).
func finalPath(p string) (string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	const fileFlagBackupSemantics = 0x02000000
	h, err := syscall.CreateFile(
		pathPtr,
		0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		fileFlagBackupSemantics,
		0,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	getFinalPath := syscall.NewLazyDLL("kernel32.dll").NewProc("GetFinalPathNameByHandleW")
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		r1, _, callErr := getFinalPath.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			0,
		)
		if r1 == 0 {
			return "", fmt.Errorf("GetFinalPathNameByHandle: %w", callErr)
		}
		if int(r1) < len(buf) {
			out := syscall.UTF16ToString(buf[:r1])
			out = strings.TrimPrefix(out, `\\?\`)
			if strings.HasPrefix(out, `UNC\`) {
				out = `\` + strings.TrimPrefix(out, `UNC`)
			}
			return out, nil
		}
		buf = make([]uint16, int(r1)+1)
	}
}
