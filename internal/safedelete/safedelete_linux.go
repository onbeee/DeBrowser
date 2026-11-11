//go:build linux

package safedelete

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// RemoveAt deletes the entry name inside the already-open,
// trusted directory fd (for example a profiles root fd held by the
// caller), with the same anchored traversal as RemoveTree. The
// caller keeps ownership of rootFd.
func RemoveAt(rootFd int, name string) error {
	return removeEntryAt(rootFd, name)
}

// RemoveVerifiedAt is RemoveAt with an identity precondition: the
// object currently bound to name inside rootFd must be exactly the
// (dev, ino) the caller verified earlier (for pfiles: the object
// MoveToQuarantine proved it placed in the quarantine slot). If
// the slot's occupant is not that object, nothing is deleted and
// an error is returned — a swapped-in substitute is never wiped
// under the verified object's authority.
func RemoveVerifiedAt(rootFd int, name string, dev, ino uint64) error {
	id, err := fstatatNoFollow(rootFd, name)
	if err != nil {
		return fmt.Errorf("safedelete: stat verified entry %q: %w", name, err)
	}
	if id.dev != dev || id.ino != ino {
		return fmt.Errorf("safedelete: entry %q identity mismatch (want dev=%d ino=%d, found dev=%d ino=%d); refusing to delete", name, dev, ino, id.dev, id.ino)
	}
	return removeEntryAt(rootFd, name)
}

// removeTree implements RemoveTree with descriptor-anchored
// traversal: openat(O_NOFOLLOW) for every descent and unlinkat for
// every removal. Once a directory fd is open, renaming or replacing
// path components in the namespace cannot redirect the walk — the
// fd pins the inode.
func removeTree(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return fmt.Errorf("safedelete: relate target: %w", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	rootFd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("safedelete: open root: %w", err)
	}
	defer func() { _ = syscall.Close(rootFd) }()

	// Descend to the target's parent, one anchored openat at a
	// time. O_NOFOLLOW makes any intermediate symlink fail the open.
	parentFd := rootFd
	for _, comp := range parts[:len(parts)-1] {
		next, err := syscall.Openat(parentFd, comp, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			if parentFd != rootFd {
				_ = syscall.Close(parentFd)
			}
			return fmt.Errorf("safedelete: descend into %q: %w", comp, err)
		}
		if parentFd != rootFd {
			_ = syscall.Close(parentFd)
		}
		parentFd = next
	}
	defer func() {
		if parentFd != rootFd {
			_ = syscall.Close(parentFd)
		}
	}()
	return removeEntryAt(parentFd, parts[len(parts)-1])
}

// entryIdentity is the (dev, ino) pair that names one filesystem
// object on Linux.
type entryIdentity struct{ dev, ino uint64 }

// fstatatNoFollow stats name inside dirFd without following a
// final symlink (AT_SYMLINK_NOFOLLOW), via the raw newfstatat
// syscall (the stdlib does not export Fstatat). It reports the
// identity of the object the NAME currently binds — the probe the
// unlink guards below compare against.
func fstatatNoFollow(dirFd int, name string) (entryIdentity, error) {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return entryIdentity{}, err
	}
	var st syscall.Stat_t
	const atSymlinkNoFollow = 0x100
	_, _, errno := syscall.Syscall6(
		sysNewfstatat,
		uintptr(dirFd),
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&st)),
		uintptr(atSymlinkNoFollow),
		0, 0,
	)
	if errno != 0 {
		return entryIdentity{}, errno
	}
	return entryIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, nil
}

// verifyEntryIdentity re-stats name inside dirFd and fails unless
// it still binds the identity captured earlier. It is checked
// immediately before every unlinkat/rmdir: a name that was swapped
// for a different object during the traversal aborts the wipe
// instead of deleting the substitute. (A swap in the single-
// syscall gap after the check cannot be excluded on Linux —
// unlinkat is name-based and there is no delete-by-fd; see the
// package documentation for the exact guarantee boundary.)
func verifyEntryIdentity(dirFd int, name string, want entryIdentity) error {
	got, err := fstatatNoFollow(dirFd, name)
	if err != nil {
		return fmt.Errorf("safedelete: re-verify %q before delete: %w", name, err)
	}
	if got != want {
		return fmt.Errorf("safedelete: entry %q was replaced during wipe (identity changed); refusing to delete the substitute", name)
	}
	return nil
}

// removeEntryAt deletes name inside the directory referenced by
// dirFd, recursing into real subdirectories only. The entry's
// identity is captured first (fstatat, no-follow), the descent is
// a single openat with O_DIRECTORY|O_NOFOLLOW, and the identity is
// re-verified immediately before the entry itself is unlinked:
//   - open succeeds   -> a real directory: recurse through its fd.
//   - ENOTDIR / ELOOP -> a file or a symlink: unlink the entry
//     itself (for a symlink, the link — never its target).
func removeEntryAt(dirFd int, name string) error {
	id, err := fstatatNoFollow(dirFd, name)
	if err != nil {
		return fmt.Errorf("safedelete: stat entry %q: %w", name, err)
	}
	childFd, err := syscall.Openat(dirFd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOTDIR || err == syscall.ELOOP {
			if verr := verifyEntryIdentity(dirFd, name, id); verr != nil {
				return verr
			}
			if uerr := unlinkat(dirFd, name, 0); uerr != nil {
				return fmt.Errorf("safedelete: unlink %q: %w", name, uerr)
			}
			return nil
		}
		return fmt.Errorf("safedelete: open entry %q: %w", name, err)
	}
	// The opened directory must be the statted object.
	var cst syscall.Stat_t
	if err := syscall.Fstat(childFd, &cst); err != nil {
		_ = syscall.Close(childFd)
		return fmt.Errorf("safedelete: fstat entry %q: %w", name, err)
	}
	if uint64(cst.Dev) != id.dev || uint64(cst.Ino) != id.ino {
		_ = syscall.Close(childFd)
		return fmt.Errorf("safedelete: entry %q was replaced during wipe (identity changed); refusing to delete the substitute", name)
	}
	// Enumerate all names first (ReadDirent advances the fd's
	// offset), then delete each child through this same anchored fd.
	names, rerr := readDirNames(childFd)
	if rerr != nil {
		_ = syscall.Close(childFd)
		return fmt.Errorf("safedelete: read dir %q: %w", name, rerr)
	}
	for _, child := range names {
		if child == "." || child == ".." {
			continue
		}
		if err := removeEntryAt(childFd, child); err != nil {
			_ = syscall.Close(childFd)
			return err
		}
	}
	_ = syscall.Close(childFd)
	if verr := verifyEntryIdentity(dirFd, name, id); verr != nil {
		return verr
	}
	if err := unlinkat(dirFd, name, atRemoveDir); err != nil {
		return fmt.Errorf("safedelete: rmdir %q: %w", name, err)
	}
	return nil
}

func readDirNames(fd int) ([]string, error) {
	var names []string
	buf := make([]byte, 8192)
	for {
		n, err := syscall.ReadDirent(fd, buf)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return names, nil
		}
		var newNames []string
		_, _, newNames = syscall.ParseDirent(buf[:n], -1, names)
		names = newNames
	}
}

// unlinkat wraps the raw unlinkat(2) syscall: the standard
// library's syscall.Unlinkat drops the flags argument (needed for
// AT_REMOVEDIR), so directory removal relative to an anchored fd
// has to go through Syscall directly. Available on Linux and
// Darwin with the same number; AT_REMOVEDIR's value comes from the
// platform's own constants.
func unlinkat(dirFd int, name string, flags int) error {
	p, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_UNLINKAT, uintptr(dirFd), uintptr(unsafe.Pointer(p)), uintptr(flags))
	if errno != 0 {
		return errno
	}
	return nil
}
