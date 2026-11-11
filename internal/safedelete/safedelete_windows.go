//go:build windows

package safedelete

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// removeTree implements RemoveTree on Windows with HANDLE-based
// deletion. The standard library has no unlinkat equivalent, so
// every object is handled like this:
//
//  1. Open it with FILE_FLAG_OPEN_REPARSE_POINT and DELETE access:
//     the handle names the object itself — for a link, the link.
//  2. Verify identity through the handle: the object must not be
//     somewhere unexpected (its GetFinalPathNameByHandle must equal
//     <verified parent final path>\<name>); a mismatch fails
//     closed and nothing is deleted.
//  3. A reparse point (symlink, junction, mount point) is deleted
//     as the link object alone, via SetFileInformationByHandle
//     (FileDispositionInfo) on its own handle — its target's
//     contents are never enumerated or touched.
//  4. A real directory's children are processed one by one under
//     the same rules (names come from a listing, but every child is
//     handle-verified before anything happens to it), and the
//     emptied directory is then disposition-deleted through its
//     handle.
func removeTree(root, target string) error {
	parentFinal, err := finalPathOfPath(filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("safedelete: resolve parent of %s: %w", target, err)
	}
	return removeEntry(target, parentFinal)
}

func removeEntry(path string, parentFinal string) error {
	h, err := openObject(path)
	if err != nil {
		return fmt.Errorf("safedelete: open %s: %w", path, err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("safedelete: stat %s: %w", path, err)
	}
	final, err := finalPathOfHandle(h)
	if err != nil {
		return fmt.Errorf("safedelete: resolve %s: %w", path, err)
	}
	expected := filepath.Join(parentFinal, filepath.Base(path))
	if !strings.EqualFold(final, expected) {
		// The object at this path is not where it claims to be (an
		// ancestor or the entry itself redirected). Fail closed.
		return fmt.Errorf("safedelete: %s resolves to %s, expected %s; refusing to delete", path, final, expected)
	}

	const fileAttributeReparsePoint = 0x00000400
	const fileAttributeDirectory = 0x00000010
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		// Link object: delete IT, never its target.
		if err := dispositionDelete(h); err != nil {
			return fmt.Errorf("safedelete: delete reparse point %s: %w", path, err)
		}
		return nil
	}
	if info.FileAttributes&fileAttributeDirectory == 0 {
		if err := dispositionDelete(h); err != nil {
			return fmt.Errorf("safedelete: delete %s: %w", path, err)
		}
		return nil
	}
	names, err := enumerateNames(path)
	if err != nil {
		return fmt.Errorf("safedelete: read dir %s: %w", path, err)
	}
	for _, name := range names {
		if err := removeEntry(filepath.Join(path, name), final); err != nil {
			return err
		}
	}
	if err := dispositionDelete(h); err != nil {
		return fmt.Errorf("safedelete: rmdir %s: %w", path, err)
	}
	return nil
}

const (
	fileFlagBackupSemantics  = 0x02000000
	fileFlagOpenReparsePoint = 0x00200000
	deleteAccess             = 0x00010000
)

// enumerateNames lists a directory's entry names with
// FindFirstFile/FindNextFile. os.ReadDir cannot be used here: the
// directory is already open with DELETE access, and a second open
// whose share mode lacks FILE_SHARE_DELETE (os.Open's mode) fails
// with a sharing violation. FindFirstFile's own open shares delete.
func enumerateNames(dir string) ([]string, error) {
	pattern, err := syscall.UTF16PtrFromString(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	var data syscall.Win32finddata
	handle, err := syscall.FindFirstFile(pattern, &data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.FindClose(handle) }()
	var names []string
	for {
		name := syscall.UTF16ToString(data.FileName[:])
		if name != "." && name != ".." {
			names = append(names, name)
		}
		if err := syscall.FindNextFile(handle, &data); err != nil {
			if err == syscall.ERROR_NO_MORE_FILES {
				return names, nil
			}
			return nil, err
		}
	}
}

// openObject opens path for deletion without following it if it is
// a reparse point.
func openObject(path string) (syscall.Handle, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(
		pathPtr,
		deleteAccess,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		fileFlagBackupSemantics|fileFlagOpenReparsePoint,
		0,
	)
}

// dispositionDelete marks the object behind h for deletion; the
// deletion takes effect when the last handle closes.
func dispositionDelete(h syscall.Handle) error {
	const fileDispositionInfo = 4
	// FILE_DISPOSITION_INFO is a single BOOLEAN.
	var flag byte = 1
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")
	r1, _, callErr := proc.Call(
		uintptr(h),
		uintptr(fileDispositionInfo),
		uintptr(unsafe.Pointer(&flag)),
		uintptr(1),
	)
	if r1 == 0 {
		return fmt.Errorf("SetFileInformationByHandle(FileDispositionInfo): %w", callErr)
	}
	return nil
}

func finalPathOfPath(p string) (string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
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
	return finalPathOfHandle(h)
}

func finalPathOfHandle(h syscall.Handle) (string, error) {
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
