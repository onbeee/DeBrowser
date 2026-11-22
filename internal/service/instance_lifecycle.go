package service

import "sync"

// InstanceLifecycle is a per-instance lifecycle mutex registry,
// shared by the instance service and the proxy service (main
// hands the SAME object to both). Instance start, stop and
// deletion and listener-port reassignment all serialize on the
// same per-instance lock, so a port can never be reassigned
// while a start is between reading the recorded port and
// launching the browser with it — and a start can never read a
// half-updated port state. Different instances never block each
// other.
//
// The lock is only ever taken at operation entry points, never
// by the asynchronous process watcher or the startup
// reconciliation: a stop operation may hold it while waiting
// for the watcher to observe the browser's exit, and the
// watcher must always be able to finish that cleanup.
//
// Entries are reference-counted and dropped when the last user
// releases, so the map does not grow with deleted instances.
// The zero value is ready to use.
type InstanceLifecycle struct {
	mu      sync.Mutex
	entries map[string]*instanceLockEntry
}

type instanceLockEntry struct {
	mu   sync.Mutex
	refs int
}

// Lock acquires the lifecycle lock for one instance and returns
// the release function.
func (l *InstanceLifecycle) Lock(id string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = map[string]*instanceLockEntry{}
	}
	e, ok := l.entries[id]
	if !ok {
		e = &instanceLockEntry{}
		l.entries[id] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.entries, id)
		}
		l.mu.Unlock()
	}
}
