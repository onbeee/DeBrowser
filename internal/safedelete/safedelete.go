// Package safedelete removes a directory tree without ever
// following a symbolic link, junction or other reparse point out of
// the tree — built for deleting browser profile directories, where
// following a planted link could destroy data outside the profiles
// directory.
//
// Why not os.RemoveAll: RemoveAll works on path strings. Between
// any check of a path and its deletion, another process can swap a
// component for a link, and RemoveAll happily recurses through
// intermediate links. This package instead ANCHORS the traversal:
//
//   - Linux: every step is relative to an already-open directory
//     file descriptor (openat with O_NOFOLLOW, unlinkat). Open fds
//     pin inodes, so namespace swaps cannot redirect the walk;
//     symlinks met anywhere are unlinked themselves, never
//     followed.
//   - Windows: the traversal is handle-anchored end to end.
//     Children are enumerated FROM a directory handle
//     (NtQueryDirectoryFile, with each child's 64-bit file ID) and
//     opened RELATIVE to that handle (NtCreateFile RootDirectory),
//     so no path string is ever re-resolved at any depth; a child
//     whose opened file index differs from its enumerated file ID
//     aborts the wipe. Reparse points are deleted as link objects
//     only. RemoveTreeFromHandle lets a caller (internal/pfiles)
//     hand over an already-verified root handle, keeping the
//     object identity chain continuous from verification to
//     deletion.
//   - Darwin (development only): the same verified-traversal model
//     as Windows, with EvalSymlinks as the resolver.
//
// The contract of RemoveTree: target must be strictly inside root
// (both absolute). It refuses anything it cannot anchor safely.
package safedelete

import (
	"fmt"
	"path/filepath"
	"strings"
)

// RemoveTree deletes the tree at target, which must lie strictly
// inside root. Links inside the tree are removed as links; their
// targets are never touched.
func RemoveTree(root, target string) error {
	cleanRoot := filepath.Clean(root)
	cleanTarget := filepath.Clean(target)
	if cleanTarget == cleanRoot || !strings.HasPrefix(cleanTarget, cleanRoot+string(filepath.Separator)) {
		return fmt.Errorf("safedelete: target %s is not strictly inside root %s", target, root)
	}
	return removeTree(cleanRoot, cleanTarget)
}
