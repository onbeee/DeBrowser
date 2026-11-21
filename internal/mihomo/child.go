package mihomo

import "os"

// childProcess is one running core process started by a Manager.
// It is the seam the process lifecycle is implemented and tested
// through: on Windows the process is created suspended, placed
// into a kill-on-close Job Object and only then resumed, so the
// core can never outlive DeBrowser — not even when DeBrowser
// crashes or is force-killed. On other platforms it is a plain
// child process. DeBrowser only ever manages processes behind
// this interface that it started itself; external cores (the
// user's Clash, a manually started Mihomo) are never adopted,
// stopped or killed.
type childProcess interface {
	// Wait blocks until the process exits and reports its exit
	// error (nil on a clean exit).
	Wait() error
	// Kill terminates the process.
	Kill() error
}

// startCoreChild starts the core executable with the given
// arguments and working directory, sending its output to logFile.
// Implemented per platform in child_unix.go / child_windows.go.
var startCoreChild func(bin string, args []string, dir string, logFile *os.File) (childProcess, error)

// StartProcess starts one core process with the same platform
// lifecycle as the managed core (on Windows: suspended creation
// into a kill-on-close Job Object). It exists for short-lived
// one-shot runs outside the Manager — the throwaway validation
// core — so those processes get the identical guarantee: they
// cannot outlive DeBrowser, and the caller owns killing and
// waiting for the returned process.
func StartProcess(bin string, args []string, dir string, logFile *os.File) (childProcess, error) {
	return startCoreChild(bin, args, dir, logFile)
}
