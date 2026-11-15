// Command debrowser starts the DeBrowser desktop application.
//
// DeBrowser is portable-first: every persistent file lives under the
// directory that contains the executable (see internal/paths).
// Startup is direct: resolve the configuration and create the
// portable directories (first launch creates and initializes them),
// set up logging, take the single-instance lock, open and migrate
// the SQLite database, open the credential vault, load settings and
// services, then run the Wails application. There is no first-launch
// choice and no scanning or migration of data from any external
// location — the Stage 1 Roaming auto-migration was removed in
// Stage 5A. Shutdown is the mirror image: the app drains its
// background loops, then the database, the instance lock and the log
// are closed in that order. Any failure before the window opens
// aborts startup with a clear error — the app never falls back to
// writing data into %APPDATA% or %LOCALAPPDATA%.
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
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/app"
	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/service"
	"github.com/onbeee/DeBrowser/internal/singleinstance"
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	application.RegisterEvent[service.Status](service.StatusEvent)
	application.RegisterEvent[service.InstallProgress](service.InstallProgressEvent)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "debrowser: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// The logging system (Stage 5B: run/operation/error categories)
	// is initialized before the database and services, and closed
	// last — this deferred close runs after the explicit database
	// close below, preserving the drain → database → log order.
	logSystem, err := logging.NewSystem(cfg)
	if err != nil {
		return err
	}
	logger := logSystem.Logger()
	defer func() { _ = logSystem.Close() }()
	logSystem.RecordLifecycle("应用启动", logging.StatusStarted, nil)
	// failStart records a structured startup-failure error event
	// (in addition to the slog record at each call site) and
	// returns the error unchanged.
	failStart := func(action string, err error) error {
		logSystem.RecordError("应用", action, err, "startup_failed", "startup", err.Error())
		return err
	}

	// The startup record deliberately carries no filesystem paths:
	// log files leave the machine in diagnostic exports, and
	// absolute paths disclose the user's directory layout.
	logger.Info("starting debrowser")

	// Single-instance protection, scoped to this portable folder.
	// A second launch never touches the database: it asks the
	// running instance to focus its window and exits successfully.
	lock, err := singleinstance.Acquire(cfg.Layout.InstanceLock)
	if err != nil {
		_ = failStart("获取单实例锁失败", err)
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
		return failStart("打开数据库失败", err)
	}
	logSystem.RecordLifecycle("数据库已打开", logging.StatusSuccess, nil)
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
		return failStart("数据库迁移失败", err)
	}
	logSystem.RecordLifecycle("数据库迁移完成", logging.StatusSuccess, nil)

	appService, err := service.NewAppService(cfg, db, logger)
	if err != nil {
		logger.Error("load settings failed", "error", err)
		return failStart("加载设置失败", err)
	}
	// Apply the user's persisted total log capacity (the logging
	// system starts with the 100 MB default). An enforcement
	// failure here is recorded by the logging system and never
	// blocks startup.
	if _, cerr := logSystem.SetCapacityLimit(appService.EffectiveLogCapacityBytes()); cerr != nil {
		logger.Warn("apply log capacity failed", "error", cerr)
	}
	instanceService, err := service.NewInstanceService(db, cfg.Layout, logger)
	if err != nil {
		logger.Error("instance service failed", "error", err)
		return failStart("初始化实例服务失败", err)
	}
	browserService := service.NewBrowserService(db, cfg.Layout, logger)

	// The credential vault opens after the database: existing
	// rows reference vault entries, and opening it at startup
	// preserves the established recovery behaviour (a vault
	// recovered from its backup, or quarantined as
	// unrecoverable, is reported loudly). The vault is a
	// data-compatibility layer only — no product feature in
	// the current scope reads or writes credentials through a
	// user-facing service, and the file is never deleted or
	// cleared by this cleanup.
	_, vaultRecovery, err := secrets.Open(cfg.Layout.SecretsFile)
	if err != nil {
		logger.Error("open credential vault failed", "error", err)
		return failStart("打开凭据 Vault 失败", err)
	}
	logSystem.RecordLifecycle("凭据 Vault 已打开", logging.StatusSuccess, nil)
	if vaultRecovery.Unrecoverable {
		logger.Warn("credential vault unrecoverable; corrupt files quarantined",
			"quarantined", vaultRecovery.Quarantined)
	} else if vaultRecovery.RecoveredFromBackup {
		logger.Warn("credential vault recovered from its backup snapshot")
	}

	// Operation audit (Stage 5B): user-initiated mutations in these
	// services record whitelist entries in the operation log.
	appService.Auditor = logSystem
	instanceService.Auditor = logSystem
	browserService.Auditor = logSystem

	diagnosticsService := service.NewDiagnosticsService(db, logSystem, appService)

	wailsApp := app.New(cfg, appService, instanceService, diagnosticsService, browserService, logger, assets)
	logSystem.RecordLifecycle("应用初始化完成", logging.StatusSuccess, nil)
	runErr := wailsApp.Run()
	if runErr != nil {
		logSystem.RecordLifecycle("应用异常退出", logging.StatusFailed, nil)
	} else {
		logSystem.RecordLifecycle("应用正常关闭", logging.StatusSuccess, nil)
	}

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
