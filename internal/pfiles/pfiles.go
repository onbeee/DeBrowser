// Package pfiles moves a verified browser profile directory into a
// quarantine slot (and back, or to deletion) with the move acting
// on the VERIFIED FILESYSTEM OBJECT, never on a re-resolved path
// string.
//
// The attack this closes: a path verified at time T proves nothing
// about what the same string names at time T+1 — a local process
// can swap any component for a symlink/junction in between, and a
// path-based rename then moves (and later deletes) an outside
// directory. So:
//
//   - Linux anchors everything to directory file descriptors:
//     openat(O_NOFOLLOW) descent from the profiles root fd, and
//     renameat2(RENAME_NOREPLACE) between two pinned fds. The
//     object moved is the object the fds name, whatever the
//     namespace does meanwhile.
//   - Windows opens the target directory with
//     FILE_FLAG_OPEN_REPARSE_POINT, verifies the handle's identity
//     (reparse attributes + final path), and renames THROUGH the
//     handle (SetFileInformationByHandle/FileRenameInfo), so the
//     verified object itself changes location.
//   - Darwin cannot offer an equivalent guarantee with the
//     standard library and fails closed instead.
//
// A Quarantine value is the capability for the moved object: it can
// be Restored (moved back, again object-anchored and
// no-replacement) or Wiped (anchored deletion), then Closed.
package pfiles

import (
	"errors"
	"path/filepath"
)

// ErrOccupied reports that the requested quarantine slot name is
// already taken; callers should pick another name and retry. The
// occupant is never overwritten or merged into.
var ErrOccupied = errors.New("pfiles: quarantine slot already occupied")

// ErrUnsafe reports that the operation could not be proven safe
// (a path component is a link, the object identity does not match,
// the platform cannot anchor the operation, ...). It always fails
// closed: nothing was moved or deleted.
var ErrUnsafe = errors.New("pfiles: operation cannot be proven safe")

// HookBeforeRename, when non-nil, runs immediately before the
// platform rename primitive inside MoveToQuarantine and Restore.
// It is a deterministic test seam for race injection (production
// code never sets it); it lets tests swap path components at the
// exact moment a path-based implementation would be vulnerable.
var HookBeforeRename func()

// Quarantine is a profile directory that has been moved into its
// quarantine slot. See the platform files for the state it holds.
type Quarantine struct {
	// ProfilesDir is the trusted profiles root (absolute, cleaned).
	ProfilesDir string
	// Name is the quarantine slot's base name inside ProfilesDir.
	Name string
	// OriginalTarget is the absolute path the directory occupied
	// before the move (its verified resolved location).
	OriginalTarget string

	platform platformState
}

// Path returns the quarantine slot's absolute path.
func (q *Quarantine) Path() string {
	return filepath.Join(q.ProfilesDir, q.Name)
}
