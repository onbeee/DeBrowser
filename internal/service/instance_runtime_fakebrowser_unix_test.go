//go:build !windows

package service

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

// On Unix the fake browser just stays alive; the runner's
// graceful close (SIGTERM) ends it with the default disposition.
func fakeBrowserBlock() {
	for {
		time.Sleep(time.Hour)
	}
}

// ignoreCloseSignals optionally makes the fake browser survive
// the runner's graceful close: with DEBROWSER_FAKE_IGNORE_TERM=1
// it ignores SIGTERM, emulating a browser that does not react
// to the close request.
func ignoreCloseSignals() {
	if os.Getenv("DEBROWSER_FAKE_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
}
