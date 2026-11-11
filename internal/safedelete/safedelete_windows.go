//go:build windows

package safedelete

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// The Windows implementation is HANDLE-ANCHORED end to end:
//
//   - The tree root enters as an already-open handle (opened by
//     the caller — pfiles hands over the very handle it verified
//     and renamed during quarantine — or, for RemoveTree, opened
//     once here and identity-checked through its final path).
//   - Directory contents are enumerated FROM the directory handle
//     with NtQueryDirectoryFile (FileIdBothDirectoryInformation),
//     which yields each child's name, attributes and 64-bit file
//     ID. FindFirstFile(path) and os.ReadDir are never used: they
//     re-resolve path strings and their opens conflict with the
//     DELETE access the traversal holds.
//   - Every child is opened RELATIVE to its verified parent handle
//     with NtCreateFile (RootDirectory = parent), so the kernel
//     resolves the name inside that exact directory object — no
//     path string is re-resolved at any depth, and replacing the
//     profiles root, the tree root's path, or any intermediate
//     path component cannot redirect the traversal.
//   - Before a child is touched, its handle's file index must
//     equal the file ID the enumeration recorded for it. A swap
//     between enumeration and open therefore aborts the wipe
//     (fail closed) instead of deleting the substitute.
//   - Reparse points are opened as link objects
//     (FILE_OPEN_REPARSE_POINT) and deleted as links only; their
//     targets are never enumerated or opened.
//   - Deletion is SetFileInformationByHandle(FileDispositionInfo)
//     on the verified handle itself.
//
// If identity cannot be established at any step (no file ID, an
// ID mismatch, an unexpected NTSTATUS), the wipe stops and reports
// the tree's recorded path; whatever remains is left in place.

// HookAfterEnumerate, when non-nil, runs after a directory's
// entries have been enumerated from its handle and before any of
// them is opened. Deterministic test seam; production never sets
// it. The argument is the directory's display path.
var HookAfterEnumerate func(dirDisplay string)

// HookAfterOpenChild, when non-nil, runs after a child object has
// been opened relative to its parent and identity-verified, before
// it is deleted or recursed into. Deterministic test seam;
// production never sets it.
var HookAfterOpenChild func(childDisplay string)

const (
	fileAttributeReparsePoint = 0x00000400
	fileAttributeDirectory    = 0x00000010

	fileListDirectory   = 0x00000001
	fileReadAttributes  = 0x00000080
	synchronizeAccess   = 0x00100000
	deleteAccess        = 0x00010000
	fileFlagBackupSem   = 0x02000000
	fileFlagOpenReparse = 0x00200000

	fileOpen              = 1 // FILE_OPEN disposition for NtCreateFile
	fileDirectoryFile     = 0x00000001
	fileNonDirectoryFile  = 0x00000040
	fileSynchronousIONonA = 0x00000020
	fileOpenReparsePoint  = 0x00200000
	objCaseInsensitive    = 0x00000040

	// NtQueryDirectoryFile takes the NT FILE_INFORMATION_CLASS
	// enum (MS-FSCC "File Information Classes"): the directory
	// query classes are FileDirectoryInformation=1,
	// FileFullDirectoryInformation=2, FileBothDirectoryInformation=3,
	// FileNamesInformation=12, FileIdBothDirectoryInformation=37,
	// FileIdFullDirectoryInformation=38. Do NOT confuse this enum
	// with the Win32 FILE_INFO_BY_HANDLE_CLASS used by
	// GetFileInformationByHandleEx / SetFileInformationByHandle
	// (where FileIdBothDirectoryInfo is 10, FileRenameInfo is 3
	// and FileDispositionInfo is 4): in the NT enum the value 10
	// is FileRenameInformation, a SET class — passing it to this
	// QUERY returns STATUS_INVALID_INFO_CLASS (0xC0000003), as
	// observed on the Windows runner.
	fileIdBothDirectoryInformation = 37

	statusSuccess        = 0x00000000
	statusNoMoreFiles    = 0x80000006
	statusObjectNameNotF = 0xC0000034
)

var (
	ntdllDLL                  = syscall.NewLazyDLL("ntdll.dll")
	procNtCreateFile          = ntdllDLL.NewProc("NtCreateFile")
	procNtQueryDirectoryFile  = ntdllDLL.NewProc("NtQueryDirectoryFile")
	procRtlNtStatusToDosError = ntdllDLL.NewProc("RtlNtStatusToDosError")
	kernel32DLL               = syscall.NewLazyDLL("kernel32.dll")
	procSetFileInfoByHandle   = kernel32DLL.NewProc("SetFileInformationByHandle")
	procGetFinalPathByHandle  = kernel32DLL.NewProc("GetFinalPathNameByHandleW")
)

// removeTree implements RemoveTree on Windows: the target is
// opened once, its identity is proven through the handle (final
// path must be exactly <parent final path>\<name>), and the
// anchored engine takes it from there.
func removeTree(root, target string) error {
	parentFinal, err := finalPathOfPath(filepath.Dir(target))
	if err != nil {
		return fmt.Errorf("safedelete: resolve parent of %s: %w", target, err)
	}
	h, err := openObject(target)
	if err != nil {
		return fmt.Errorf("safedelete: open %s: %w", target, err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	final, err := finalPathOfHandle(h)
	if err != nil {
		return fmt.Errorf("safedelete: resolve %s: %w", target, err)
	}
	expected := filepath.Join(parentFinal, filepath.Base(target))
	if !strings.EqualFold(final, expected) {
		return fmt.Errorf("safedelete: %s resolves to %s, expected %s; refusing to delete", target, final, expected)
	}
	return RemoveTreeFromHandle(h, target)
}

// RemoveTreeFromHandle deletes the CONTENTS of the directory
// object behind root — and finally marks root itself for
// deletion — without ever resolving a path. root stays owned by
// the caller (it is NOT closed here); the deletion of root itself
// completes when the caller closes it. displayPath is used only
// for error messages: it is the tree's recorded location, so a
// failure report can name the residual quarantine directory.
//
// The wipe fails closed: on any identity doubt it stops, deletes
// nothing further, and returns an error naming displayPath.
// Partial progress is possible before such a failure (entries
// deleted earlier stay deleted); the error says so by naming the
// residual tree rather than claiming a rollback.
func RemoveTreeFromHandle(root syscall.Handle, displayPath string) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(root, &info); err != nil {
		return fmt.Errorf("safedelete: stat root of %s: %w", displayPath, err)
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		// The root itself is a link object: delete it alone.
		if err := dispositionDelete(root); err != nil {
			return fmt.Errorf("safedelete: delete reparse root %s: %w", displayPath, err)
		}
		return nil
	}
	if info.FileAttributes&fileAttributeDirectory == 0 {
		if err := dispositionDelete(root); err != nil {
			return fmt.Errorf("safedelete: delete %s: %w", displayPath, err)
		}
		return nil
	}
	if err := wipeChildren(root, displayPath); err != nil {
		return err
	}
	if err := dispositionDelete(root); err != nil {
		return fmt.Errorf("safedelete: rmdir %s: %w", displayPath, err)
	}
	return nil
}

// dirEntry is one enumerated child: the name as stored, its
// attributes, and its 64-bit file ID — the identity the opened
// child handle must match.
type dirEntry struct {
	name   string
	attrs  uint32
	fileID uint64
}

// wipeChildren deletes every entry of the directory behind dirH.
// It works in passes — enumerate a snapshot from the handle, then
// delete the snapshot's objects by handle — and re-enumerates
// between passes, so entries deleted in an earlier pass are gone
// from the listing and anything that could not be deleted aborts
// the wipe instead of being chased by path.
func wipeChildren(dirH syscall.Handle, display string) error {
	const maxPasses = 4
	for pass := 0; ; pass++ {
		entries, err := enumerateFromHandle(dirH)
		if err != nil {
			return fmt.Errorf("safedelete: enumerate %s: %w", display, err)
		}
		if len(entries) == 0 {
			return nil
		}
		if pass >= maxPasses {
			return fmt.Errorf("safedelete: %s still holds %d entries after %d passes; refusing to continue, tree left at %s", display, len(entries), maxPasses, display)
		}
		if HookAfterEnumerate != nil {
			HookAfterEnumerate(display)
		}
		for _, e := range entries {
			if err := wipeEntry(dirH, e, filepath.Join(display, e.name)); err != nil {
				return err
			}
		}
	}
}

// wipeEntry opens one enumerated child RELATIVE to its verified
// parent handle, proves the opened object is the enumerated one
// (file index equality), and deletes it — recursing first when it
// is a real directory.
func wipeEntry(parentH syscall.Handle, e dirEntry, display string) error {
	if e.fileID == 0 {
		return fmt.Errorf("safedelete: %s: enumeration provided no file ID; cannot prove identity, refusing to delete (tree left at %s)", display, display)
	}
	h, vanished, err := openChildRelative(parentH, e)
	if err != nil {
		return fmt.Errorf("safedelete: open %s: %w", display, err)
	}
	if vanished {
		// The enumerated object no longer exists under that name;
		// nothing was substituted by us and nothing is deleted.
		return nil
	}
	defer func() { _ = syscall.CloseHandle(h) }()

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("safedelete: stat %s: %w", display, err)
	}
	openedID := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	if openedID != e.fileID {
		// The name was swapped between enumeration and open: the
		// object behind the handle is NOT the enumerated one. Fail
		// closed — delete nothing, including not this substitute.
		return fmt.Errorf("safedelete: %s: object identity changed during wipe (enumerated file ID %d, opened %d); refusing to delete, tree left in place", display, e.fileID, openedID)
	}
	if HookAfterOpenChild != nil {
		HookAfterOpenChild(display)
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		// Link object: delete IT, never its target.
		if err := dispositionDelete(h); err != nil {
			return fmt.Errorf("safedelete: delete reparse point %s: %w", display, err)
		}
		return nil
	}
	if info.FileAttributes&fileAttributeDirectory == 0 {
		if err := dispositionDelete(h); err != nil {
			return fmt.Errorf("safedelete: delete %s: %w", display, err)
		}
		return nil
	}
	if err := wipeChildren(h, display); err != nil {
		return err
	}
	if err := dispositionDelete(h); err != nil {
		return fmt.Errorf("safedelete: rmdir %s: %w", display, err)
	}
	return nil
}

// enumerateFromHandle lists dirH's children with
// NtQueryDirectoryFile(FileIdBothDirectoryInformation), starting
// a fresh scan. Entries deleted since a previous scan simply do
// not appear.
func enumerateFromHandle(dirH syscall.Handle) ([]dirEntry, error) {
	buf := make([]byte, 64*1024)
	var entries []dirEntry
	restart := uintptr(1)
	for {
		var iosb ioStatusBlock
		status, _ := ntCall(procNtQueryDirectoryFile,
			uintptr(dirH),
			0, 0, 0,
			uintptr(unsafe.Pointer(&iosb)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			uintptr(fileIdBothDirectoryInformation),
			0, // ReturnSingleEntry = FALSE
			0, // FileName = NULL (all entries)
			restart,
		)
		restart = 0
		if status == statusNoMoreFiles {
			return entries, nil
		}
		if status != statusSuccess {
			return nil, fmt.Errorf("NtQueryDirectoryFile: %s", ntStatusError(status))
		}
		n := int(iosb.Information)
		if n == 0 || n > len(buf) {
			return entries, nil
		}
		chunk := buf[:n]
		for {
			if len(chunk) < 104 {
				return nil, fmt.Errorf("NtQueryDirectoryFile: truncated directory entry")
			}
			next := int(binary.LittleEndian.Uint32(chunk[0:4]))
			attrs := binary.LittleEndian.Uint32(chunk[56:60])
			nameLen := int(binary.LittleEndian.Uint32(chunk[60:64])) // bytes
			fileID := binary.LittleEndian.Uint64(chunk[96:104])
			if 104+nameLen > len(chunk) {
				return nil, fmt.Errorf("NtQueryDirectoryFile: entry name overruns buffer")
			}
			nameUTF16 := make([]uint16, nameLen/2)
			for i := range nameUTF16 {
				nameUTF16[i] = binary.LittleEndian.Uint16(chunk[104+2*i : 106+2*i])
			}
			name := syscall.UTF16ToString(nameUTF16)
			if name != "." && name != ".." {
				entries = append(entries, dirEntry{name: name, attrs: attrs, fileID: fileID})
			}
			if next == 0 {
				break
			}
			if next < 104 || next > len(chunk) {
				return nil, fmt.Errorf("NtQueryDirectoryFile: invalid entry offset %d", next)
			}
			chunk = chunk[next:]
		}
	}
}

type unicodeString struct {
	Length        uint16
	MaximumLength uint16
	_             uint32
	Buffer        *uint16
}

type objectAttributes struct {
	Length                   uint32
	_                        uint32
	RootDirectory            syscall.Handle
	ObjectName               *unicodeString
	Attributes               uint32
	_                        uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

type ioStatusBlock struct {
	Status      uintptr
	Information uintptr
}

// openChildRelative opens e inside the directory behind parentH
// with NtCreateFile(RootDirectory = parentH). The kernel resolves
// the name within that directory object only. FILE_OPEN_REPARSE_
// POINT makes a link child open as the link itself. vanished
// reports STATUS_OBJECT_NAME_NOT_FOUND: the enumerated entry no
// longer exists (it was removed, not substituted by us).
func openChildRelative(parentH syscall.Handle, e dirEntry) (h syscall.Handle, vanished bool, err error) {
	nameUTF16, err := syscall.UTF16FromString(e.name)
	if err != nil {
		return 0, false, err
	}
	us := unicodeString{
		Length:        uint16(2 * (len(nameUTF16) - 1)),
		MaximumLength: uint16(2 * len(nameUTF16)),
		Buffer:        &nameUTF16[0],
	}
	oa := objectAttributes{
		Length:        48,
		RootDirectory: parentH,
		ObjectName:    &us,
		Attributes:    objCaseInsensitive,
	}
	access := uint32(deleteAccess | fileReadAttributes | synchronizeAccess)
	options := uint32(fileSynchronousIONonA | fileOpenReparsePoint)
	if e.attrs&fileAttributeDirectory != 0 {
		access |= fileListDirectory
		options |= fileDirectoryFile
	} else {
		options |= fileNonDirectoryFile
	}
	var iosb ioStatusBlock
	var handle syscall.Handle
	status, _ := ntCall(procNtCreateFile,
		uintptr(unsafe.Pointer(&handle)),
		uintptr(access),
		uintptr(unsafe.Pointer(&oa)),
		uintptr(unsafe.Pointer(&iosb)),
		0, // AllocationSize
		0, // FileAttributes
		uintptr(syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE),
		uintptr(fileOpen),
		uintptr(options),
		0, // EaBuffer
		0, // EaLength
	)
	if status == statusObjectNameNotF {
		return 0, true, nil
	}
	if status != statusSuccess {
		return 0, false, fmt.Errorf("NtCreateFile(%s): %s", e.name, ntStatusError(status))
	}
	return handle, false, nil
}

// ntCall invokes an ntdll procedure and returns its NTSTATUS.
func ntCall(proc *syscall.LazyProc, args ...uintptr) (uintptr, error) {
	r1, _, callErr := proc.Call(args...)
	return r1, callErr
}

func ntStatusError(status uintptr) error {
	r1, _, _ := procRtlNtStatusToDosError.Call(status)
	if r1 != 0 && r1 != 0xFFFFFFFF {
		return fmt.Errorf("NTSTATUS 0x%08X (Win32 error %d: %w)", status, r1, syscall.Errno(r1))
	}
	return fmt.Errorf("NTSTATUS 0x%08X", status)
}

// dispositionDelete marks the object behind h for deletion; the
// deletion takes effect when the last handle closes.
func dispositionDelete(h syscall.Handle) error {
	const fileDispositionInfo = 4
	// FILE_DISPOSITION_INFO is a single BOOLEAN.
	var flag byte = 1
	r1, _, callErr := procSetFileInfoByHandle.Call(
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

// openObject opens path for anchored deletion without following
// it if it is a reparse point. The access mask includes
// FILE_LIST_DIRECTORY so the handle can drive enumeration.
func openObject(path string) (syscall.Handle, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(
		pathPtr,
		deleteAccess|fileListDirectory|fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		fileFlagBackupSem|fileFlagOpenReparse,
		0,
	)
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
		fileFlagBackupSem,
		0,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	return finalPathOfHandle(h)
}

func finalPathOfHandle(h syscall.Handle) (string, error) {
	buf := make([]uint16, syscall.MAX_PATH)
	for {
		r1, _, callErr := procGetFinalPathByHandle.Call(
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
