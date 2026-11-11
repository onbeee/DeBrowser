//go:build !windows

package singleinstance

import (
	"errors"
	"os"
	"syscall"
)

// tryLock attempts a non-blocking exclusive flock on f. It returns
// errLocked when another process (or another open file description
// in this process) already holds it. The lock is bound to the open
// file description and is released by the OS automatically when the
// process dies — including SIGKILL and power loss — so no stale lock
// can survive its owner.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errLocked
		}
		return err
	}
	return nil
}

// unlock releases the flock. Deleting the lock file is deliberately
// NOT part of release: unlinking a locked file would let a newcomer
// create and lock a different inode while a third process still
// holds the old one.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
