package service

import (
	"errors"
	"path/filepath"
	"sync"

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
	// BookmarkGateBusy means the instance is mid start/stop
	// transition or another bookmark operation already holds it:
	// the caller must not touch the file now.
	BookmarkGateBusy = "busy"
)

// gateForLocked decides the gate from an instance's effective
// status. hasRuntimeRecord refines the persisted "error" state:
// with no runtime record the launch failed before a process
// existed, so the instance counts as confirmed stopped; with a
// record its state stays unknown. Callers hold rtMu.
func (s *InstanceService) gateForLocked(inst model.Instance, hasRuntimeRecord bool) string {
	switch s.effectiveStatusLocked(inst) {
	case model.StatusStopped:
		return BookmarkGateStopped
	case model.StatusRunning, model.StatusStarting, model.StatusStopping:
		return BookmarkGateRunning
	case model.StatusError:
		if !hasRuntimeRecord {
			return BookmarkGateStopped
		}
		return BookmarkGateUnknown
	default:
		return BookmarkGateUnknown
	}
}

// InstanceBookmarkAccess resolves an instance for bookmark file
// operations: the instance, its absolute profile directory and
// the gate state. It is a status query only — it does NOT hold
// the instance. Callers that actually read or write the file must
// use acquireBookmarkAccess so no start/stop can interleave.
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
	_, rerr := s.store.GetInstanceRuntime(c, id)
	s.rtMu.Lock()
	gate := s.gateForLocked(inst, rerr == nil)
	s.rtMu.Unlock()
	return inst, dir, gate, nil
}

// acquireBookmarkAccess atomically checks an instance's state and,
// when it is confirmed stopped, takes the instance's bookmark hold
// so the caller can read and write its bookmarks file with no
// possible interleaving: while the hold lasts, StartInstance and
// StopInstance refuse the instance, and no second bookmark
// operation can acquire it. The check and the hold happen inside
// one rtMu critical section — there is no window between them.
//
// On success the gate is BookmarkGateStopped and release is a
// non-nil, idempotent function the caller must defer. On any other
// gate (running, unknown, busy) release is nil and nothing is
// held. An in-flight start/stop transition reports busy, as does
// an instance already held by another bookmark operation.
func (s *InstanceService) acquireBookmarkAccess(id string) (inst model.Instance, dir string, gate string, release func(), err error) {
	c, cancel := ctx()
	defer cancel()
	inst, err = s.store.GetInstance(c, id)
	if errors.Is(err, store.ErrNotFound) {
		return model.Instance{}, "", BookmarkGateUnknown, nil, errors.New("实例不存在")
	}
	if err != nil {
		return model.Instance{}, "", BookmarkGateUnknown, nil, err
	}
	dir = filepath.Join(s.layout.Data, filepath.FromSlash(inst.UserDataDir))
	// Read the runtime record before taking rtMu: an in-flight
	// transition that could create one is visible through the
	// transient/procs checks below, so this value cannot go stale
	// in a way that matters.
	_, rerr := s.store.GetInstanceRuntime(c, id)
	hasRecord := rerr == nil

	s.rtMu.Lock()
	if s.bookmarkHolds[id] {
		s.rtMu.Unlock()
		return inst, dir, BookmarkGateBusy, nil, nil
	}
	if _, inTransition := s.transient[id]; inTransition {
		s.rtMu.Unlock()
		return inst, dir, BookmarkGateBusy, nil, nil
	}
	if gate := s.gateForLocked(inst, hasRecord); gate != BookmarkGateStopped {
		s.rtMu.Unlock()
		return inst, dir, gate, nil, nil
	}
	s.bookmarkHolds[id] = true
	s.rtMu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			s.rtMu.Lock()
			delete(s.bookmarkHolds, id)
			s.rtMu.Unlock()
		})
	}
	return inst, dir, BookmarkGateStopped, release, nil
}
