//go:build !windows

package service

import "time"

// On Unix the fake browser just stays alive; the runner's
// graceful close (SIGTERM) ends it with the default disposition.
func fakeBrowserBlock() {
	for {
		time.Sleep(time.Hour)
	}
}
