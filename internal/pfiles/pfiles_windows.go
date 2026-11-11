//go:build windows

package pfiles

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/onbeee/DeBrowser/internal/safedelete"
)

// platformState holds the handle of the moved directory itself.
// The rename acts through this handle, so once the object has been
// verified, no namespace change can substitute another object.
type platformState struct {
	handle         syscall.Handle
	resolvedParent string // final path of the target's parent at verification time
}

const (
	fileAttributeReparsePoint = 0x00000400
	fileFlagBackupSemantics   = 0x02000000
	fileFlagOpenReparsePoint  = 0x00200000
	deleteAccess              = 0x00010000
	errAlreadyExists          = 183
	errFileExists             = 80
)

var procSetFileInfoByHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetFileInformationByHandle")

// MoveToQuarantine moves the directory at target into
// profilesDir/quarantineName by renaming the VERIFIED HANDLE:
//
//  1. The profiles root is opened as a reparse point; if the root
//     itself is a reparse point, refuse.
//  2. The parent directory is opened the same way and its final
//     path recorded — this is the identity the restore later
//     re-checks.
//  3. The target is opened with FILE_FLAG_OPEN_REPARSE_POINT (the
//     handle names the object itself, never a link target). It is
//     refused when it carries the reparse attribute or when its
//     final path is not exactly <resolved parent>\<name> — the
//     signature of an intermediate component redirecting it.
//  4. SetFileInformationByHandle(FileRenameInfo,
//     ReplaceIfExists=FALSE) renames the handle's object into the
//     quarantine slot. The namespace state of the source path at
//     this moment is irrelevant: the OS moves the object the
//     handle refers to.
func MoveToQuarantine(profilesDir, target, quarantineName string) (*Quarantine, error) {
	cleanRoot := filepath.Clean(profilesDir)
	cleanTarget := filepath.Clean(target)
	// Containment must be judged on RESOLVED paths: callers may
	// hold the profiles root in 8.3 short form (C:\Users\RUNNER~1\
	// from %TEMP%) while the target arrives resolved to long form —
	// a lexical Rel between the two forms fails even though the
	// target is plainly inside. Canonicalize the root through its
	// own final path first.
	resolvedRoot, err := finalPathOfPath(cleanRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve profiles root: %v", ErrUnsafe, err)
	}
	cleanRoot = resolvedRoot
	// NOTE: no lexical Rel gate here. Between mixed name forms
	// (8.3 short vs long) a lexical Rel reports ".." escapes for
	// paths that are plainly inside, so it cannot distinguish a
	// real escape from a form mismatch. The authoritative
	// containment proof happens below, entirely on resolved paths:
	// resolvedParent must lie within resolvedRoot, and the
	// target's final path must equal <resolvedParent>\<name>.

	rootHandle, err := openDirHandle(cleanRoot, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open profiles root: %v", ErrUnsafe, err)
	}
	rootInfo, err := fileInfo(rootHandle)
	_ = syscall.CloseHandle(rootHandle)
	if err != nil {
		return nil, fmt.Errorf("pfiles: stat profiles root: %w", err)
	}
	if rootInfo.FileAttributes&fileAttributeReparsePoint != 0 {
		return nil, fmt.Errorf("%w: profiles root is a reparse point", ErrUnsafe)
	}

	parent := filepath.Dir(cleanTarget)
	parentHandle, err := openDirHandle(parent, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open parent directory: %v", ErrUnsafe, err)
	}
	parentInfo, err := fileInfo(parentHandle)
	resolvedParent, ferr := finalPathOf(parentHandle)
	_ = syscall.CloseHandle(parentHandle)
	if err != nil {
		return nil, fmt.Errorf("pfiles: stat parent: %w", err)
	}
	if ferr != nil {
		return nil, fmt.Errorf("pfiles: resolve parent: %w", ferr)
	}
	if parentInfo.FileAttributes&fileAttributeReparsePoint != 0 {
		// The parent itself is a link: the target's real location is
		// wherever it points, which the caller should have resolved
		// beforehand. Refuse rather than guess.
		return nil, fmt.Errorf("%w: parent directory is a reparse point", ErrUnsafe)
	}
	// Authoritative containment: the target's RESOLVED parent must
	// be the resolved profiles root or inside it (segment-boundary
	// prefix, case-insensitive). Together with the check below that
	// the target's final path is exactly <resolved parent>\<name>,
	// this proves the object being opened lives inside profiles.
	if !strings.EqualFold(resolvedParent, cleanRoot) &&
		!strings.HasPrefix(strings.ToLower(resolvedParent), strings.ToLower(cleanRoot)+`\`) {
		return nil, fmt.Errorf("%w: target's resolved parent %s is not inside profiles %s", ErrUnsafe, resolvedParent, cleanRoot)
	}

	targetHandle, err := openDirHandle(cleanTarget, deleteAccess)
	if err != nil {
		return nil, fmt.Errorf("%w: open target: %v", ErrUnsafe, err)
	}
	fail := func(err error) (*Quarantine, error) {
		_ = syscall.CloseHandle(targetHandle)
		return nil, err
	}
	targetInfo, err := fileInfo(targetHandle)
	if err != nil {
		return fail(fmt.Errorf("pfiles: stat target: %w", err))
	}
	if targetInfo.FileAttributes&fileAttributeReparsePoint != 0 {
		return fail(fmt.Errorf("%w: target itself is a reparse point", ErrUnsafe))
	}
	resolvedTarget, err := finalPathOf(targetHandle)
	if err != nil {
		return fail(fmt.Errorf("pfiles: resolve target: %w", err))
	}
	expected := filepath.Join(resolvedParent, filepath.Base(cleanTarget))
	if !strings.EqualFold(resolvedTarget, expected) {
		return fail(fmt.Errorf("%w: target resolves to %s, expected %s (an intermediate component redirected it)", ErrUnsafe, resolvedTarget, expected))
	}

	quarantinePath := filepath.Join(cleanRoot, quarantineName)
	if HookBeforeRename != nil {
		HookBeforeRename()
	}
	if err := renameByHandle(targetHandle, quarantinePath); err != nil {
		return fail(err)
	}
	// Post-move proof: the destination path itself was resolved by
	// the OS at rename time, so verify through the handle that the
	// object really landed at the quarantine slot (a component of
	// the destination swapped mid-rename would show up here). If
	// not, move it back and refuse.
	landed, err := finalPathOf(targetHandle)
	if err != nil {
		return fail(fmt.Errorf("pfiles: resolve moved object: %w", err))
	}
	if !strings.EqualFold(landed, quarantinePath) {
		_ = renameByHandle(targetHandle, cleanTarget)
		return fail(fmt.Errorf("%w: moved object landed at %s, expected %s", ErrUnsafe, landed, quarantinePath))
	}

	q := &Quarantine{ProfilesDir: cleanRoot, Name: quarantineName, OriginalTarget: cleanTarget}
	q.platform.handle = targetHandle
	q.platform.resolvedParent = resolvedParent
	return q, nil
}

// Restore renames the quarantined object back to its original
// path, again through the object's handle. Before renaming, the
// original parent's identity is re-verified: if the parent path
// now resolves somewhere else (it was swapped for a junction), the
// restore refuses and the object stays safely in quarantine.
func (q *Quarantine) Restore() error {
	parent := filepath.Dir(q.OriginalTarget)
	parentHandle, err := openDirHandle(parent, 0)
	if err != nil {
		return fmt.Errorf("%w: reopen parent for restore: %v", ErrUnsafe, err)
	}
	parentInfo, ierr := fileInfo(parentHandle)
	resolvedParent, ferr := finalPathOf(parentHandle)
	_ = syscall.CloseHandle(parentHandle)
	if ierr != nil {
		return fmt.Errorf("pfiles: stat parent for restore: %w", ierr)
	}
	// The parent was a real directory at move time. If it now
	// carries the reparse attribute it has been swapped for a link
	// — GetFinalPathNameByHandle on the link's own handle returns
	// the link's location and would NOT reveal this, so the
	// attribute check is the authoritative one. Fail closed.
	if parentInfo.FileAttributes&fileAttributeReparsePoint != 0 {
		return fmt.Errorf("%w: original parent is now a reparse point; profile left in quarantine %s", ErrUnsafe, q.Path())
	}
	if ferr != nil {
		return fmt.Errorf("pfiles: resolve parent for restore: %w", ferr)
	}
	if !strings.EqualFold(resolvedParent, q.platform.resolvedParent) {
		return fmt.Errorf("%w: original parent now resolves to %s, expected %s; profile left in quarantine %s", ErrUnsafe, resolvedParent, q.platform.resolvedParent, q.Path())
	}
	if HookBeforeRename != nil {
		HookBeforeRename()
	}
	if err := renameByHandle(q.platform.handle, q.OriginalTarget); err != nil {
		return err
	}
	// Same post-move proof as MoveToQuarantine — compared in
	// RESOLVED form on both sides (the stored OriginalTarget may
	// use the 8.3 short form while the resolver returns long form;
	// the resolved parent captured at move time is the anchor).
	landed, err := finalPathOf(q.platform.handle)
	if err != nil {
		return fmt.Errorf("pfiles: resolve restored object: %w", err)
	}
	expected := filepath.Join(q.platform.resolvedParent, filepath.Base(q.OriginalTarget))
	if !strings.EqualFold(landed, expected) {
		_ = renameByHandle(q.platform.handle, q.Path())
		return fmt.Errorf("%w: restored object landed at %s, expected %s; moved back to quarantine %s", ErrUnsafe, landed, expected, q.Path())
	}
	return nil
}

// Wipe deletes the quarantined tree with safedelete's
// handle-verified traversal. Our own handle is closed first.
func (q *Quarantine) Wipe() error {
	if q.platform.handle != 0 {
		_ = syscall.CloseHandle(q.platform.handle)
		q.platform.handle = 0
	}
	return safedelete.RemoveTree(q.ProfilesDir, q.Path())
}

// Close releases the object's handle.
func (q *Quarantine) Close() error {
	if q.platform.handle != 0 {
		_ = syscall.CloseHandle(q.platform.handle)
		q.platform.handle = 0
	}
	return nil
}

// openDirHandle opens a directory WITHOUT following it if it is a
// reparse point (the handle then names the point itself).
func openDirHandle(path string, access uint32) (syscall.Handle, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(
		pathPtr,
		access,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		fileFlagBackupSemantics|fileFlagOpenReparsePoint,
		0,
	)
}

func fileInfo(h syscall.Handle) (*syscall.ByHandleFileInformation, error) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return nil, err
	}
	return &info, nil
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
		fileFlagBackupSemantics|fileFlagOpenReparsePoint,
		0,
	)
	if err != nil {
		return "", err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	return finalPathOf(h)
}

func finalPathOf(h syscall.Handle) (string, error) {
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

// renameByHandle renames the object behind h to destPath via
// SetFileInformationByHandle(FileRenameInfo) with
// ReplaceIfExists=FALSE: an occupied destination fails with
// ErrOccupied instead of being overwritten.
func renameByHandle(h syscall.Handle, destPath string) error {
	const fileRenameInfo = 3
	nameUTF16, err := syscall.UTF16FromString(destPath)
	if err != nil {
		return err
	}
	// The name is stored without its NUL terminator; FileNameLength
	// counts bytes.
	nameBytes := uint32(2 * (len(nameUTF16) - 1))
	// FILE_RENAME_INFO layout (x64): BOOLEAN ReplaceIfExists at 0
	// (with the union's padding, RootDirectory lands at offset 8),
	// DWORD FileNameLength at 16, and the WCHAR FileName at offset
	// 20 — NOT 24: a 24-byte header would prepend a NUL to the name
	// and the call fails with ERROR_INVALID_NAME.
	buf := make([]byte, 20+2*len(nameUTF16))
	// buf[0] = 0 (ReplaceIfExists = FALSE); RootDirectory = NULL.
	*(*uint32)(unsafe.Pointer(&buf[16])) = nameBytes
	for i, c := range nameUTF16 {
		*(*uint16)(unsafe.Pointer(&buf[20+2*i])) = c
	}
	r1, _, callErr := procSetFileInfoByHandle.Call(
		uintptr(h),
		uintptr(fileRenameInfo),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r1 == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && (errno == errAlreadyExists || errno == errFileExists) {
			return ErrOccupied
		}
		return fmt.Errorf("pfiles: SetFileInformationByHandle(FileRenameInfo): %w", callErr)
	}
	return nil
}
