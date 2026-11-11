//go:build !windows

package singleinstance

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid belongs to a live process, using
// signal 0: it performs error checking without delivering a signal.
// EPERM means the process exists but belongs to another user, which
// still counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
