//go:build linux

package pfiles

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/onbeee/DeBrowser/internal/safedelete"
)

// platformState holds the two pinned directory descriptors the
// whole protocol runs on: the profiles root (the anchor) and the
// target's parent directory (the rename source/destination). While
// these fds are open, no namespace change can redirect an operation
// into another directory.
type platformState struct {
	rootFd   int
	parentFd int
}

// MoveToQuarantine moves the directory at target (which must lie
// strictly inside profilesDir, both absolute) into
// profilesDir/quarantineName, acting only on descriptor-anchored
// objects:
//
//  1. The profiles root is opened O_NOFOLLOW — if the root itself
//     is a symlink, the open fails and nothing happens.
//  2. Every intermediate component is descended with
//     openat(O_NOFOLLOW|O_DIRECTORY); a symlinked component fails.
//  3. The target is opened the same way and fstat'ed: it must be a
//     real directory, and its (dev, ino) identity is recorded.
//  4. renameat2(RENAME_NOREPLACE) moves (parentFd, base) to
//     (rootFd, quarantineName). After the move, the object now at
//     the quarantine slot is fstat-verified to be the SAME (dev,
//     ino) object that was anchored in step 3.
func MoveToQuarantine(profilesDir, target, quarantineName string) (*Quarantine, error) {
	cleanRoot := filepath.Clean(profilesDir)
	cleanTarget := filepath.Clean(target)
	rel, err := filepath.Rel(cleanRoot, cleanTarget)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("%w: target %s is not inside profiles %s", ErrUnsafe, target, profilesDir)
	}
	parts := strings.Split(rel, string(filepath.Separator))

	rootFd, err := syscall.Open(cleanRoot, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open profiles root: %v", ErrUnsafe, err)
	}
	fail := func(err error) (*Quarantine, error) {
		_ = syscall.Close(rootFd)
		return nil, err
	}

	// Descend to the target's parent, anchored at every level.
	parentFd := rootFd
	ownParent := false
	for _, comp := range parts[:len(parts)-1] {
		next, err := syscall.Openat(parentFd, comp, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			if ownParent {
				_ = syscall.Close(parentFd)
			}
			return fail(fmt.Errorf("%w: descend into %q: %v", ErrUnsafe, comp, err))
		}
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		parentFd = next
		ownParent = true
	}
	base := parts[len(parts)-1]

	// Anchor the target itself and record its identity.
	targetFd, err := syscall.Openat(parentFd, base, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(fmt.Errorf("%w: open target: %v", ErrUnsafe, err))
	}
	var before syscall.Stat_t
	if err := syscall.Fstat(targetFd, &before); err != nil {
		_ = syscall.Close(targetFd)
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(fmt.Errorf("pfiles: fstat target: %w", err))
	}
	_ = syscall.Close(targetFd)
	if before.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(fmt.Errorf("%w: target is not a directory", ErrUnsafe))
	}

	if HookBeforeRename != nil {
		HookBeforeRename()
	}
	if err := renameat2NoReplace(parentFd, base, rootFd, quarantineName); err != nil {
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(err)
	}

	// Prove the moved object is the anchored object: open the
	// quarantine slot through the pinned root fd (stdlib has no
	// Fstatat; an anchored open + Fstat is equivalent) and compare
	// identities.
	var after syscall.Stat_t
	fd, oerr := syscall.Openat(rootFd, quarantineName, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if oerr != nil {
		note := fmt.Errorf("pfiles: verify moved object: %v (quarantine slot %s may hold the profile; refusing to proceed)", oerr, quarantineName)
		_ = renameat2NoReplace(rootFd, quarantineName, parentFd, base)
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(note)
	}
	ferr := syscall.Fstat(fd, &after)
	_ = syscall.Close(fd)
	if ferr != nil {
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(fmt.Errorf("pfiles: fstat moved object: %w", ferr))
	}
	if after.Dev != before.Dev || after.Ino != before.Ino {
		// The rename moved something else — this cannot happen with
		// fd-anchored renameat, but fail closed all the same and try
		// to put it back.
		_ = renameat2NoReplace(rootFd, quarantineName, parentFd, base)
		if ownParent {
			_ = syscall.Close(parentFd)
		}
		return fail(fmt.Errorf("%w: moved object identity mismatch (quarantine %s)", ErrUnsafe, quarantineName))
	}

	q := &Quarantine{ProfilesDir: cleanRoot, Name: quarantineName, OriginalTarget: cleanTarget}
	q.platform.rootFd = rootFd
	if ownParent {
		q.platform.parentFd = parentFd
	} else {
		// Target directly under the root: the parent IS the root.
		q.platform.parentFd = rootFd
	}
	return q, nil
}

// Restore moves the quarantined directory back to its original
// location: renameat2(RENAME_NOREPLACE) between the same two pinned
// fds. If the original name has been recreated meanwhile, the
// rename refuses (ErrOccupied) instead of overwriting it.
func (q *Quarantine) Restore() error {
	base := filepath.Base(q.OriginalTarget)
	if HookBeforeRename != nil {
		HookBeforeRename()
	}
	if err := renameat2NoReplace(q.platform.rootFd, q.Name, q.platform.parentFd, base); err != nil {
		return err
	}
	return nil
}

// Wipe deletes the quarantined tree through the pinned root fd
// (safedelete's anchored traversal).
func (q *Quarantine) Wipe() error {
	return safedelete.RemoveAt(q.platform.rootFd, q.Name)
}

// Close releases the pinned descriptors.
func (q *Quarantine) Close() error {
	if q.platform.parentFd != 0 && q.platform.parentFd != q.platform.rootFd {
		_ = syscall.Close(q.platform.parentFd)
	}
	if q.platform.rootFd != 0 {
		_ = syscall.Close(q.platform.rootFd)
	}
	q.platform.parentFd, q.platform.rootFd = 0, 0
	return nil
}

// renameat2NoReplace performs renameat2(2) with RENAME_NOREPLACE
// between two directory fds. The stdlib does not export Renameat2,
// so it goes through the raw syscall; kernels >= 3.15 all have it.
func renameat2NoReplace(oldDirFd int, oldName string, newDirFd int, newName string) error {
	oldPtr, err := syscall.BytePtrFromString(oldName)
	if err != nil {
		return err
	}
	newPtr, err := syscall.BytePtrFromString(newName)
	if err != nil {
		return err
	}
	const renameNoReplace = 1
	_, _, errno := syscall.Syscall6(
		sysRenameat2,
		uintptr(oldDirFd),
		uintptr(unsafe.Pointer(oldPtr)),
		uintptr(newDirFd),
		uintptr(unsafe.Pointer(newPtr)),
		uintptr(renameNoReplace),
		0,
	)
	if errno == 0 {
		return nil
	}
	if errno == syscall.EEXIST || errno == syscall.ENOTEMPTY {
		return ErrOccupied
	}
	if errno == syscall.ENOSYS {
		return fmt.Errorf("%w: renameat2 unsupported by this kernel", ErrUnsafe)
	}
	return fmt.Errorf("pfiles: renameat2: %w", errno)
}
