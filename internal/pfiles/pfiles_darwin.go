//go:build darwin

package pfiles

import "fmt"

// platformState is empty on Darwin: no quarantine can be created.
type platformState struct{}

// MoveToQuarantine fails closed on Darwin. The verified-object
// move this package exists for needs descriptor-relative rename
// (openat/renameat) or handle-based rename, and the Go standard
// library exposes neither on Darwin (syscall.Openat does not exist
// there). A path-based rename would reintroduce exactly the
// verify/use race this package removes on Linux and Windows, so
// the destructive profile operation refuses here instead. Darwin
// is a development platform only; DeBrowser ships for Windows and
// its CI exercises the Linux and Windows implementations.
func MoveToQuarantine(profilesDir, target, quarantineName string) (*Quarantine, error) {
	return nil, fmt.Errorf("%w: macOS cannot anchor profile deletion to file descriptors/handles with the standard library; refusing to delete", ErrUnsafe)
}

func (q *Quarantine) Restore() error {
	return fmt.Errorf("%w: no quarantine exists on this platform", ErrUnsafe)
}

func (q *Quarantine) Wipe() error {
	return fmt.Errorf("%w: no quarantine exists on this platform", ErrUnsafe)
}

func (q *Quarantine) Close() error { return nil }
