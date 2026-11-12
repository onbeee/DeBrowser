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
//     followed. Each entry's (dev, ino) identity is captured
//     before it is touched and re-verified immediately before it
//     is unlinked, so a substituted object aborts the wipe.
//   - Windows: the traversal is handle-anchored end to end.
//     Children are enumerated FROM a directory handle
//     (GetFileInformationByHandleEx with the extended directory
//     classes, yielding each child's 128-bit file ID) and opened
//     RELATIVE to that handle (NtCreateFile RootDirectory), so no
//     path string is ever re-resolved at any depth; a child whose
//     opened 128-bit identity (volume serial + FileId, via
//     FileIdInfo) differs from its enumerated identity aborts the
//     wipe. Child handles are opened without FILE_SHARE_DELETE,
//     so an opened object cannot be renamed out of the tree by
//     another process while the wipe holds it. Reparse points are
//     deleted as link objects only. RemoveTreeFromHandle lets a
//     caller (internal/pfiles) hand over an already-verified root
//     handle, keeping the object identity chain continuous from
//     verification to deletion.
//   - Darwin (development only): the same verified-traversal model
//     as Windows, with EvalSymlinks as the resolver.
//
// Guarantee boundary (Windows identity): the 128-bit file ID
// check is contemporaneous — it proves the object opened just
// after enumeration is the object enumeration saw (on NTFS an
// object keeps its file ID until deleted). Microsoft documents
// that file systems may reuse file IDs after deletion, so the ID
// is not a permanent, cross-deletion object credential and is
// never used as one here; the enumerate→open window is the whole
// exposure, and the no-FILE_SHARE_DELETE opens close it once the
// object is held.
//
// Guarantee boundary (Linux): descriptor-anchored deletion follows
// VERIFIED OBJECTS (inodes), and identity is re-verified before
// every unlink — an object that was never verified inside the
// boundary cannot be deleted, and a substituted name aborts the
// wipe. Linux has no share-mode equivalent, though: renaming an
// already-open directory is always permitted, so a same-UID
// process that can rename entries inside the tree can move a
// verified object out mid-wipe, and deletion then follows that
// object (its contents are deleted through its pinned fd wherever
// it now sits). Likewise unlinkat is name-based — there is no
// delete-by-fd — so a final single-syscall window remains between
// the last identity check and the unlink itself. Both residuals
// require a hostile same-UID process with write access inside the
// profile tree, which is outside this application's single-user
// desktop threat model; Windows closes both via share modes.
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
