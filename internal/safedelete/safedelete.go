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
//   - Windows: every directory is verified through its handle's
//     final path (GetFinalPathNameByHandle) before its children are
//     touched; an entry that resolves anywhere other than its own
//     path is a reparse point and is removed as a link object only.
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
