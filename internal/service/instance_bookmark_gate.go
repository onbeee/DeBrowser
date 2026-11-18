package service

import (
	"errors"
	"path/filepath"

	"github.com/onbeee/DeBrowser/internal/chromebookmarks"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Bookmark file access gate states (Stage 9C). Reading or writing
// a profile's Chromium Bookmarks file is only safe while the
// browser is confirmed stopped: a running browser owns the file in
// memory and overwrites external edits, and an instance whose
// process state cannot be confirmed must be treated the same way.
const (
	BookmarkGateStopped = "stopped"
	BookmarkGateRunning = "running"
	BookmarkGateUnknown = "unknown"
)

// InstanceBookmarkAccess resolves an instance for bookmark file
// operations: the instance, its absolute profile directory and the
// gate state. The gate reuses the runtime's effective status —
// never the persisted status alone — plus the runtime record: an
// instance in the persisted "error" state with no runtime record
// has no process (its launch failed before one existed), so it
// counts as confirmed stopped; with a record it stays unknown.
func (s *InstanceService) InstanceBookmarkAccess(id string) (model.Instance, string, string, error) {
	c, cancel := ctx()
	defer cancel()
	inst, err := s.store.GetInstance(c, id)
	if errors.Is(err, store.ErrNotFound) {
		return model.Instance{}, "", BookmarkGateUnknown, errors.New("实例不存在")
	}
	if err != nil {
		return model.Instance{}, "", BookmarkGateUnknown, err
	}
	dir := filepath.Join(s.layout.Data, filepath.FromSlash(inst.UserDataDir))
	switch s.effectiveStatus(inst) {
	case model.StatusStopped:
		return inst, dir, BookmarkGateStopped, nil
	case model.StatusRunning, model.StatusStarting, model.StatusStopping:
		return inst, dir, BookmarkGateRunning, nil
	case model.StatusError:
		if _, rerr := s.store.GetInstanceRuntime(c, id); errors.Is(rerr, store.ErrNotFound) {
			return inst, dir, BookmarkGateStopped, nil
		}
		return inst, dir, BookmarkGateUnknown, nil
	default:
		return inst, dir, BookmarkGateUnknown, nil
	}
}

// bookmarkProfileDir resolves an instance's user data directory
// (the second return of InstanceBookmarkAccess) to the actual
// Chromium profile directory holding its Bookmarks file. The
// Bookmarks file lives in a profile subdirectory of the user data
// directory, never in the user data directory itself. EVERY
// bookmark file entry point — counting, sync, search, edit,
// delete — resolves through here so they all read and write the
// same profile; see chromebookmarks.ResolveProfileDir for the
// resolution rules.
func bookmarkProfileDir(userDataDir string) (string, error) {
	return chromebookmarks.ResolveProfileDir(userDataDir)
}
