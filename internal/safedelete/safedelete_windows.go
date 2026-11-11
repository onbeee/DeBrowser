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
//     with GetFileInformationByHandleEx and the extended directory
//     information classes (FileIdExtdDirectoryRestartInfo for the
//     first call of a scan, FileIdExtdDirectoryInfo to continue),
//     which yield each child's name, attributes and 128-bit file
//     ID. FindFirstFile(path) and os.ReadDir are never used: they
//     re-resolve path strings and their opens conflict with the
//     DELETE access the traversal holds.
//   - Every child is opened RELATIVE to its verified parent handle
//     with NtCreateFile (RootDirectory = parent), so the kernel
//     resolves the name inside that exact directory object — no
//     path string is re-resolved at any depth, and replacing the
//     profiles root, the tree root's path, or any intermediate
//     path component cannot redirect the traversal.
//   - Before a child is touched, its handle's 128-bit identity
//     (volume serial number + FileId, read back with
//     GetFileInformationByHandleEx(FileIdInfo)) must equal the
//     identity the enumeration recorded for it, on the same volume
//     as its parent. A swap between enumeration and open therefore
//     aborts the wipe (fail closed) instead of deleting the
//     substitute. Where a filesystem cannot provide the extended
//     classes or a 128-bit identity, the wipe refuses to run at
//     all — it never degrades to a weaker check.
//   - Child handles are opened WITHOUT FILE_SHARE_DELETE: while
//     the wipe holds an object open, no other process can rename
//     or move it out of the tree (an attempt fails with a sharing
//     violation instead of silently relocating the object the
//     handle refers to).
//   - Reparse points are opened as link objects
//     (FILE_OPEN_REPARSE_POINT) and deleted as links only; their
//     targets are never enumerated or opened.
//   - Deletion is SetFileInformationByHandle(FileDispositionInfo)
//     on the verified handle itself.
//
// If identity cannot be established at any step (no file ID, an
// ID mismatch, an unexpected NTSTATUS), the wipe stops and reports
// the tree's recorded path; whatever remains is left in place.
//
// Scope of the identity guarantee (stated precisely, per the
// Microsoft documentation for BY_HANDLE_FILE_INFORMATION /
// FILE_ID_INFO): a file ID plus volume serial uniquely identifies
// a file on a computer and is the documented way to decide
// whether two handles name the same file; on NTFS an object keeps
// its file ID until it is deleted. File IDs are NOT permanent
// credentials — the documentation states file systems are free to
// reuse them, so an ID can resurface on a different object after
// the original is deleted. The check here is therefore a
// CONTEMPORANEOUS one: it proves the object opened a moment after
// enumeration is the object enumeration saw. Defeating it would
// require deleting the enumerated object AND recreating a
// same-named object that the filesystem assigns the identical
// 128-bit ID, all inside the enumerate→open window; once the
// object is open, the share modes above close the window for
// good. Anything short of that proof fails closed.

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

	// GetFileInformationByHandleEx takes the Win32
	// FILE_INFO_BY_HANDLE_CLASS enum (winbase.h) — a DIFFERENT
	// enum from the NT FILE_INFORMATION_CLASS used by the NT
	// native API; never mix the two families. The classes used
	// here (documented in the GetFileInformationByHandleEx and
	// FILE_ID_EXTD_DIR_INFO / FILE_ID_INFO references):
	//   FileIdInfo                    = 18  (FILE_ID_INFO)
	//   FileIdExtdDirectoryInfo       = 19  (FILE_ID_EXTD_DIR_INFO)
	//   FileIdExtdDirectoryRestartInfo= 20  (same, restarts scan)
	fileIdInfoClass                     = 18
	fileIdExtdDirectoryInfoClass        = 19
	fileIdExtdDirectoryRestartInfoClass = 20

	// FILE_ID_EXTD_DIR_INFO fixed header size: all fields through
	// the 16-byte FileId at offset 72, with FileName at offset 88.
	extdDirInfoHeader = 88

	// Win32 error returned by GetFileInformationByHandleEx when a
	// directory scan is exhausted (or the directory is empty).
	errorNoMoreFiles = 18 // ERROR_NO_MORE_FILES

	statusSuccess        = 0x00000000
	statusObjectNameNotF = 0xC0000034
)

var (
	ntdllDLL                  = syscall.NewLazyDLL("ntdll.dll")
	procNtCreateFile          = ntdllDLL.NewProc("NtCreateFile")
	procRtlNtStatusToDosError = ntdllDLL.NewProc("RtlNtStatusToDosError")
	kernel32DLL               = syscall.NewLazyDLL("kernel32.dll")
	procSetFileInfoByHandle   = kernel32DLL.NewProc("SetFileInformationByHandle")
	procGetFinalPathByHandle  = kernel32DLL.NewProc("GetFinalPathNameByHandleW")
	procGetFileInfoByHandleEx = kernel32DLL.NewProc("GetFileInformationByHandleEx")
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
// attributes, and its 128-bit file ID — the identity the opened
// child handle must match (together with the parent directory's
// volume serial number).
type dirEntry struct {
	name   string
	attrs  uint32
	fileID [16]byte
}

// fileIdentity is a filesystem object's full identity on Windows:
// the volume serial number plus its 128-bit file ID, as reported
// by GetFileInformationByHandleEx(FileIdInfo) (FILE_ID_INFO:
// VolumeSerialNumber uint64 at offset 0, FileId FILE_ID_128 at
// offset 8, 24 bytes total).
type fileIdentity struct {
	volume uint64
	id     [16]byte
}

// identityOf reads h's 128-bit identity. Any failure is fatal to
// the caller: without a verifiable identity the wipe must not
// proceed (no silent downgrade to name/attribute checks).
func identityOf(h syscall.Handle) (fileIdentity, error) {
	var buf [24]byte
	r1, _, callErr := procGetFileInfoByHandleEx.Call(
		uintptr(h),
		uintptr(fileIdInfoClass),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r1 == 0 {
		return fileIdentity{}, fmt.Errorf("GetFileInformationByHandleEx(FileIdInfo): %w", callErr)
	}
	var id fileIdentity
	id.volume = binary.LittleEndian.Uint64(buf[0:8])
	copy(id.id[:], buf[8:24])
	return id, nil
}

// wipeChildren deletes every entry of the directory behind dirH.
// It works in passes — enumerate a snapshot from the handle, then
// delete the snapshot's objects by handle — and re-enumerates
// between passes, so entries deleted in an earlier pass are gone
// from the listing and anything that could not be deleted aborts
// the wipe instead of being chased by path.
func wipeChildren(dirH syscall.Handle, display string) error {
	// The parent's own identity anchors the volume every child
	// must live on; if the filesystem cannot provide it, no child
	// identity can be proven and the wipe refuses to start.
	parentID, err := identityOf(dirH)
	if err != nil {
		return fmt.Errorf("safedelete: identity of %s: %w", display, err)
	}
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
			if err := wipeEntry(dirH, parentID.volume, e, filepath.Join(display, e.name)); err != nil {
				return err
			}
		}
	}
}

// wipeEntry opens one enumerated child RELATIVE to its verified
// parent handle, proves the opened object is the enumerated one
// (128-bit identity equality on the parent's volume), and deletes
// it — recursing first when it is a real directory.
func wipeEntry(parentH syscall.Handle, parentVolume uint64, e dirEntry, display string) error {
	if e.fileID == ([16]byte{}) {
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
	openedID, err := identityOf(h)
	if err != nil {
		return fmt.Errorf("safedelete: identity of %s: %w", display, err)
	}
	if openedID.volume != parentVolume || openedID.id != e.fileID {
		// The name was swapped between enumeration and open: the
		// object behind the handle is NOT the enumerated one. Fail
		// closed — delete nothing, including not this substitute.
		return fmt.Errorf("safedelete: %s: object identity changed during wipe (enumerated file ID %x on volume %d, opened %x on volume %d); refusing to delete, tree left in place", display, e.fileID, parentVolume, openedID.id, openedID.volume)
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

// enumerateFromHandle lists dirH's children FROM the handle with
// GetFileInformationByHandleEx: the first call of a scan uses
// FileIdExtdDirectoryRestartInfo (restarts the scan), subsequent
// calls use FileIdExtdDirectoryInfo until the API reports
// ERROR_NO_MORE_FILES. A scan of an empty directory ends the same
// way on the first call, which is the ONLY way an empty result is
// produced — any other failure (including filesystems that do not
// support the extended classes) is an error, never an empty list.
//
// GetFileInformationByHandleEx does not report how many bytes it
// wrote; the entry chain is self-terminating (the last entry's
// NextEntryOffset is 0), and every field access is bounds-checked
// against the buffer, so bytes beyond the chain are never read.
func enumerateFromHandle(dirH syscall.Handle) ([]dirEntry, error) {
	var entries []dirEntry
	class := uintptr(fileIdExtdDirectoryRestartInfoClass)
	for {
		buf := make([]byte, 64*1024)
		r1, _, callErr := procGetFileInfoByHandleEx.Call(
			uintptr(dirH),
			class,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
		)
		class = fileIdExtdDirectoryInfoClass
		if r1 == 0 {
			if callErr == syscall.Errno(errorNoMoreFiles) {
				return entries, nil
			}
			return nil, fmt.Errorf("GetFileInformationByHandleEx(FileIdExtdDirectoryInfo): %w", callErr)
		}
		walked, err := parseExtdDirEntries(buf, &entries)
		if err != nil {
			return nil, err
		}
		if walked == 0 {
			// A successful call that yields no entry at all is not
			// a valid scan state; refuse rather than mistake it
			// for an empty directory.
			return nil, fmt.Errorf("GetFileInformationByHandleEx: successful call returned no directory entries; refusing to treat the directory as empty")
		}
	}
}

// parseExtdDirEntries walks one buffer of FILE_ID_EXTD_DIR_INFO
// records (Microsoft layout: NextEntryOffset@0, FileAttributes@56,
// FileNameLength@60 in BYTES, EaSize@64, ReparsePointTag@68,
// FileId@72 (16 bytes), FileName@88 as WCHAR[1]) and appends the
// entries to out, skipping "." and "..". It returns how many
// records were walked. Parsing is strict and overflow-safe: a
// truncated header, an odd or over-long file name, an empty name,
// or a NextEntryOffset that is too small, misaligned, or out of
// the buffer is an error — nothing is silently skipped. A
// non-terminal record must additionally CONTAIN its own file
// name: NextEntryOffset >= 88 + FileNameLength, so a name can
// never run past its record into the next one (overlapping
// records); the terminal record's name must end inside the
// buffer, which the overrun check above enforces.
func parseExtdDirEntries(buf []byte, out *[]dirEntry) (int, error) {
	walked := 0
	off := 0
	for {
		remaining := len(buf) - off
		if remaining < extdDirInfoHeader {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: truncated directory entry header (%d bytes left)", remaining)
		}
		e := buf[off:]
		next := binary.LittleEndian.Uint32(e[0:4])
		attrs := binary.LittleEndian.Uint32(e[56:60])
		nameLen := binary.LittleEndian.Uint32(e[60:64]) // bytes
		if nameLen == 0 {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: directory entry with an empty file name")
		}
		if nameLen%2 != 0 {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: directory entry with odd file name length %d", nameLen)
		}
		if uint64(extdDirInfoHeader)+uint64(nameLen) > uint64(remaining) {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: entry name overruns buffer")
		}
		var fileID [16]byte
		copy(fileID[:], e[72:88])
		nameUTF16 := make([]uint16, nameLen/2)
		for i := range nameUTF16 {
			nameUTF16[i] = binary.LittleEndian.Uint16(e[extdDirInfoHeader+2*i : extdDirInfoHeader+2*i+2])
		}
		name := syscall.UTF16ToString(nameUTF16)
		walked++
		if name != "." && name != ".." {
			*out = append(*out, dirEntry{name: name, attrs: attrs, fileID: fileID})
		}
		if next == 0 {
			return walked, nil
		}
		if uint64(next) < uint64(extdDirInfoHeader)+uint64(nameLen) {
			// The record's own file name would run past the start
			// of the next record: the records overlap, which no
			// well-formed scan produces. All arithmetic is uint64,
			// so a near-2^32 FileNameLength cannot wrap this check.
			return walked, fmt.Errorf("GetFileInformationByHandleEx: NextEntryOffset %d cuts through the record's own file name (header %d + name %d bytes); records would overlap", next, extdDirInfoHeader, nameLen)
		}
		if next < extdDirInfoHeader {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: invalid NextEntryOffset %d", next)
		}
		if next%8 != 0 {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: NextEntryOffset %d is not 8-byte aligned", next)
		}
		if uint64(next) > uint64(remaining) {
			return walked, fmt.Errorf("GetFileInformationByHandleEx: NextEntryOffset %d overruns buffer", next)
		}
		off += int(next)
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
		Length:        uint32(unsafe.Sizeof(objectAttributes{})),
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
		// ShareAccess deliberately EXCLUDES FILE_SHARE_DELETE:
		// while this handle is open, no other process can rename
		// or move the object (such an attempt fails with a sharing
		// violation), so a verified object can never be relocated
		// outside the tree behind the wipe's back and then deleted
		// "by handle" at its new location. If an external handle
		// already prevents this open, NtCreateFile fails and the
		// wipe aborts (fail closed) with the tree left in place.
		uintptr(syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE),
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
