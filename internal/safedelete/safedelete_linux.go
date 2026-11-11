//go:build linux

package safedelete

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

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

// removeEntryAt deletes name inside the directory referenced by
// dirFd, recursing into real subdirectories only. The probe is a
// single openat with O_DIRECTORY|O_NOFOLLOW — there is no separate
// stat step an attacker could race:
//   - open succeeds   -> a real directory: recurse through its fd.
//   - ENOTDIR / ELOOP -> a file or a symlink: unlink the entry
//     itself (for a symlink, the link — never its target).
func removeEntryAt(dirFd int, name string) error {
	childFd, err := syscall.Openat(dirFd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOTDIR || err == syscall.ELOOP {
			if uerr := unlinkat(dirFd, name, 0); uerr != nil {
				return fmt.Errorf("safedelete: unlink %q: %w", name, uerr)
			}
			return nil
		}
		return fmt.Errorf("safedelete: open entry %q: %w", name, err)
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
