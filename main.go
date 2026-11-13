// Command debrowser starts the DeBrowser desktop application.
//
// DeBrowser is portable-first: every persistent file lives under the
// directory that contains the executable (see internal/paths).
//
// Startup is two-phase (Stage 5A). Phase one only RESOLVES the
// layout and classifies the installation (internal/installstate) —
// it creates nothing. An initialized installation takes the normal
// startup: create/verify directories, set up logging, take the
// single-instance lock, open and migrate the SQLite database, open
// the credential vault, load services, run the Wails application.
// An uninitialized installation shows the first-launch welcome window
// instead: it runs entirely out of a temporary workspace in the OS
// temp directory (WebView2 data, log, single-instance coordination),
// so the portable folder stays exe-only until the user chooses
// "新用户", which runs the initialization (internal/initialize) and
// then restarts this executable into the normal startup. An
// interrupted initialization is detected by its marker and cleaned
// up before the welcome window is shown again.
//
// Shutdown of the normal startup is the mirror image: the app drains
// its background loops, then the database, the instance lock and the
// log are closed in that order. Any failure before the window opens
// aborts startup with a clear error — the app never falls back to
// writing data into %APPDATA% or %LOCALAPPDATA%, and it never scans
// or migrates data from any external location.
//
// main is a thin wrapper over run: run returns errors instead of
// calling log.Fatalf, so every deferred cleanup always executes.
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/app"
	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/service"
	"github.com/onbeee/DeBrowser/internal/singleinstance"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	application.RegisterEvent[service.Status](service.StatusEvent)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "debrowser: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Phase one: resolve only. Nothing is created on disk yet, so an
	// uninitialized installation stays exactly as it was downloaded.
	cfg, err := config.Resolve(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	switch installstate.Detect(cfg.Layout) {
	case installstate.PhaseReady:
		return runFormal(cfg)
	case installstate.PhaseInitializing:
		// A previous new-user initialization was interrupted (crash,
		// power loss, killed process). Its marker proves no completed
		// initialization ever produced user data, so its partial
		// artifacts are removed and the user starts over from the
		// welcome page. A cleanup failure is reported, never hidden.
		if err := installstate.CleanupFormal(cfg.Layout); err != nil {
			return fmt.Errorf("清理上次未完成的初始化失败，无法继续启动：%w", err)
		}
		fmt.Fprintln(os.Stderr, "检测到上次初始化未完成，已清理其残留文件。")
		return runWelcome(cfg)
	default:
		return runWelcome(cfg)
	}
}

// runFormal is the normal startup of an initialized installation.
func runFormal(cfg config.Config) error {
	if err := cfg.Layout.Ensure(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	logger, closeLog, err := logging.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()

	logger.Info("starting debrowser",
		"root", cfg.Layout.Root,
		"dataDir", cfg.Layout.Data,
		"database", cfg.Layout.DatabaseFile,
	)

	// Single-instance protection, scoped to this portable folder.
	// A second launch never touches the database: it asks the
	// running instance to focus its window and exits successfully.
	lock, err := singleinstance.Acquire(cfg.Layout.InstanceLock)
	if err != nil {
		var already *singleinstance.AlreadyRunningError
		if errors.As(err, &already) {
			if serr := singleinstance.SignalActivation(cfg.Layout.ActivationFile); serr != nil {
				logger.Warn("could not signal the running instance", "error", serr)
			}
			logger.Info("another instance is already running; activation signaled",
				"pid", already.PID)
			fmt.Fprintf(os.Stderr, "DeBrowser 已在运行（进程 %d），已尝试唤起现有窗口。\n", already.PID)
			return nil
		}
		logger.Error("single-instance lock failed", "error", err)
		return err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			logger.Warn("release instance lock failed", "error", rerr)
		}
	}()

	db, err := database.OpenWithOptions(cfg.Layout.DatabaseFile, database.Options{
		BackupDir: cfg.Layout.Backups,
	})
	if err != nil {
		logger.Error("open database failed", "error", err)
		return err
	}
	// The database is closed explicitly after the application stops
	// (below); this deferred guard covers the early-return paths in
	// between, and runs exactly once thanks to the flag.
	dbClosed := false
	defer func() {
		if !dbClosed {
			_ = db.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.Migrate(ctx)
	cancel()
	if err != nil {
		logger.Error("database migration failed", "error", err)
		return err
	}

	// Installations created before Stage 5A have no marker file yet:
	// record the completed state once the database has opened and
	// migrated successfully. Best effort — a marker write failure
	// must not block a healthy startup, and the database check in
	// installstate.Detect keeps classifying the install as ready.
	if _, exists, _ := installstate.ReadMarker(cfg.Layout); !exists {
		if err := installstate.MarkComplete(cfg.Layout); err != nil {
			logger.Warn("record install marker failed", "error", err)
		}
	}

	appService, err := service.NewAppService(cfg, db, logger)
	if err != nil {
		logger.Error("load settings failed", "error", err)
		return err
	}
	instanceService, err := service.NewInstanceService(db, cfg.Layout, logger)
	if err != nil {
		logger.Error("instance service failed", "error", err)
		return err
	}

	// The credential vault opens after the database: proxy rows
	// reference vault entries, and the proxy service's startup
	// garbage collection needs both. A vault that cannot be opened
	// at all (permissions cannot be established, I/O failure)
	// aborts startup — silently continuing would risk treating
	// stored credentials as absent. A vault recovered from its
	// backup, or quarantined as unrecoverable, is reported loudly
	// and surfaces per-proxy as credential-unavailable.
	vault, vaultRecovery, err := secrets.Open(cfg.Layout.SecretsFile)
	if err != nil {
		logger.Error("open credential vault failed", "error", err)
		return err
	}
	if vaultRecovery.Unrecoverable {
		logger.Warn("credential vault unrecoverable; corrupt files quarantined, affected proxies report credential-unavailable",
			"quarantined", vaultRecovery.Quarantined)
	} else if vaultRecovery.RecoveredFromBackup {
		logger.Warn("credential vault recovered from its backup snapshot")
	}

	proxyService, err := service.NewProxyService(db, vault, logger)
	if err != nil {
		logger.Error("proxy service failed", "error", err)
		return err
	}

	wailsApp := app.New(cfg, appService, instanceService, proxyService, logger, assets)
	runErr := wailsApp.Run()

	// The app has fully stopped and its background loops are drained:
	// nothing can touch the database any more, so close it now.
	dbClosed = true
	if cerr := db.Close(); cerr != nil {
		logger.Error("close database failed", "error", cerr)
		if runErr == nil {
			runErr = cerr
		}
	}
	if runErr != nil {
		logger.Error("application exited with error", "error", runErr)
	}
	return runErr
}

// runWelcome is the first-launch flow of an uninitialized
// installation. Everything it needs at runtime — WebView2 user data,
// the log file and the single-instance coordination files — lives in
// a per-installation workspace under the OS temp directory and is
// removed when the welcome process exits; the portable folder is not
// touched until the user chooses "新用户" and initialization runs.
// After a successful initialization the formal application is
// started as a new process (which takes the normal startup path) and
// the welcome process exits, so there is exactly one initialization
// and no in-process service swapping.
func runWelcome(cfg config.Config) error {
	ws, err := installstate.TempWorkspace(cfg.Layout.Root)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(ws.Dir) }()

	logCfg := cfg
	logCfg.Layout.LogFile = ws.LogFile
	logger, closeLog, err := logging.New(logCfg)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()

	logger.Info("starting debrowser welcome (installation not initialized)",
		"root", cfg.Layout.Root,
		"workspace", ws.Dir,
	)

	// Single-instance protection for the welcome flow, scoped to
	// this installation via its temp workspace: a second launch
	// focuses the open welcome window instead of racing a second
	// initialization.
	lock, err := singleinstance.Acquire(ws.LockFile)
	if err != nil {
		var already *singleinstance.AlreadyRunningError
		if errors.As(err, &already) {
			if serr := singleinstance.SignalActivation(ws.ActivationFile); serr != nil {
				logger.Warn("could not signal the running welcome window", "error", serr)
			}
			logger.Info("welcome window already open; activation signaled", "pid", already.PID)
			fmt.Fprintf(os.Stderr, "DeBrowser 首次启动页面已在运行（进程 %d），已尝试唤起现有窗口。\n", already.PID)
			return nil
		}
		logger.Error("welcome single-instance lock failed", "error", err)
		return err
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			logger.Warn("release welcome lock failed", "error", rerr)
		}
	}()

	var welcomeApp *app.WelcomeApp
	welcomeService := service.NewWelcomeService(cfg.Layout, logger, func() {
		if err := startFormalProcess(); err != nil {
			// Initialization DID complete and its marker is written:
			// the next launch takes the normal path. Keep the
			// welcome window open so the frontend's completion view
			// can tell the user to relaunch manually.
			logger.Error("start formal process after initialization failed", "error", err)
			return
		}
		logger.Info("initialization complete; formal process started, welcome exiting")
		welcomeApp.Quit()
	})
	welcomeApp = app.NewWelcome(cfg, welcomeService, ws, logger, assets)
	return welcomeApp.Run()
}

// startFormalProcess launches this executable again; the new process
// sees the completed install marker and takes the normal startup.
func startFormalProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = filepath.Dir(exe)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start debrowser: %w", err)
	}
	return nil
}
