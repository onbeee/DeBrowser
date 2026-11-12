// Command debrowser starts the DeBrowser desktop application.
//
// DeBrowser is portable-first: every persistent file lives under the
// directory that contains the executable (see internal/paths). Startup
// order: resolve configuration and directories, set up logging, take
// the single-instance lock, migrate any Stage 1 roaming data, open
// and migrate the SQLite database, load settings and services, then
// run the Wails application. Shutdown is the mirror image: the app
// drains its background loops, then the database, the instance lock
// and the log are closed in that order. Any failure before the window
// opens aborts startup with a clear error — the app never falls back
// to writing data into %APPDATA% or %LOCALAPPDATA%.
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
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/app"
	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/migration"
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
	cfg, err := config.Load(os.Getenv)
	if err != nil {
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

	// One-time migration of Stage 1 roaming data. Runs before the
	// database is opened; a failure aborts startup so the user never
	// sees a silently created empty database while their real data
	// still sits in the legacy location.
	legacyRoot, err := migration.DefaultLegacyRoot()
	if err != nil {
		logger.Error("legacy data detection failed", "error", err)
		return err
	}
	if filepath.Clean(legacyRoot) != filepath.Clean(cfg.Layout.Root) {
		report, err := migration.Run(cfg.Layout, legacyRoot)
		if err != nil {
			logger.Error("legacy data migration failed", "error", err)
			return fmt.Errorf("legacy data migration failed: %w", err)
		}
		if report.Needed {
			logger.Info("legacy data migrated",
				"databaseCopied", report.DatabaseCopied,
				"filesCopied", report.FilesCopied,
				"filesSkipped", report.FilesSkipped,
				"legacyRoot", report.LegacyRoot,
			)
		}
	}

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

	wailsApp := app.New(cfg, appService, instanceService, logger, assets)
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
