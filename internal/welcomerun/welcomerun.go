// Package welcomerun owns the runtime lifecycle of the Stage 5A
// welcome mode: the temporary workspace, its log file and its
// single-instance lock, and — critically — WHO is allowed to clean
// the workspace up.
//
// Ownership protocol:
//   - Creating the workspace (TempWorkspace) is idempotent and
//     confers NO ownership: the deterministic per-installation
//     directory may belong to a welcome process that is already
//     running.
//   - Only the process that successfully acquires the workspace's
//     single-instance lock owns it. Only the owner, on exit, closes
//     the log, releases the lock and removes the workspace — in
//     that order.
//   - A process that finds the lock held signals the running
//     welcome window to focus and exits WITHOUT touching the
//     workspace: the running instance's WebView2 data and lock file
//     stay exactly as they are.
//   - Failures before the lock is acquired (workspace or log
//     creation) likewise remove nothing.
package welcomerun

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/singleinstance"
)

// ServeFunc runs the welcome window (or, in tests, a stand-in) while
// the caller holds the workspace lock. The workspace and the logger
// writing into it are provided; the workspace is removed by Run
// after ServeFunc returns, never before.
type ServeFunc func(ws installstate.Workspace, log *slog.Logger) error

// Run executes the welcome mode for an uninitialized installation:
// resolve the temp workspace, open its log, take its single-instance
// lock, and serve. It returns nil when another welcome window was
// already running (it was signaled to focus instead) or when serve
// completed; serve's error is propagated after cleanup.
func Run(cfg config.Config, serve ServeFunc) error {
	ws, err := installstate.TempWorkspace(cfg.Layout.Root)
	if err != nil {
		return err
	}

	logCfg := cfg
	logCfg.Layout.LogFile = ws.LogFile
	logger, closeLog, err := logging.New(logCfg)
	if err != nil {
		return err
	}

	lock, err := singleinstance.Acquire(ws.LockFile)
	if err != nil {
		var already *singleinstance.AlreadyRunningError
		if errors.As(err, &already) {
			// Another welcome process owns this workspace. Signal
			// it, report, and leave EVERYTHING in place: no lock
			// release (it is not ours), no workspace removal.
			if serr := singleinstance.SignalActivation(ws.ActivationFile); serr != nil {
				logger.Warn("could not signal the running welcome window", "error", serr)
			}
			logger.Info("welcome window already open; activation signaled", "pid", already.PID)
			_ = closeLog()
			fmt.Fprintf(os.Stderr, "DeBrowser 首次启动页面已在运行（进程 %d），已尝试唤起现有窗口。\n", already.PID)
			return nil
		}
		_ = closeLog()
		return err
	}

	// This process owns the workspace from here on. Exit order:
	// close the log, release the lock, then remove the workspace.
	defer func() {
		_ = closeLog()
		if rerr := lock.Release(); rerr != nil {
			fmt.Fprintf(os.Stderr, "释放首次启动单实例锁失败：%v\n", rerr)
		}
		_ = os.RemoveAll(ws.Dir)
	}()

	logger.Info("starting debrowser welcome (installation not initialized)",
		"root", cfg.Layout.Root,
		"workspace", ws.Dir,
	)
	return serve(ws, logger)
}
