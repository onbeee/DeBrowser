//go:build windows

package singleinstance

import "os"

// processAlive reports whether pid belongs to a live process. On
// Windows os.FindProcess opens a handle to the process and fails for
// PIDs that do not exist, which is exactly the liveness signal the
// lock needs. (Unix-style signal 0 does not exist here.)
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
