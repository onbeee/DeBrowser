// Command debrowser starts the DeBrowser desktop application.
//
// DeBrowser is portable-first: every persistent file lives under the
// directory that contains the executable (see internal/paths). Startup
// order: resolve configuration and directories, set up logging, migrate
// any Stage 1 roaming data, open and migrate the SQLite database, load
// settings, then run the Wails application. Any failure before the
// window opens aborts startup with a clear error — the app never falls
// back to writing data into %APPDATA% or %LOCALAPPDATA%.
package main

import (
	"context"
	"embed"
	"log"
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
)

//go:embed all:frontend/dist
var assets embed.FS

func init() {
	application.RegisterEvent[service.Status](service.StatusEvent)
}

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatalf("debrowser: configuration: %v", err)
	}

	logger, closeLog, err := logging.New(cfg)
	if err != nil {
		log.Fatalf("debrowser: %v", err)
	}
	defer func() { _ = closeLog() }()

	logger.Info("starting debrowser",
		"root", cfg.Layout.Root,
		"dataDir", cfg.Layout.Data,
		"database", cfg.Layout.DatabaseFile,
	)

	// One-time migration of Stage 1 roaming data. Runs before the
	// database is opened; a failure aborts startup so the user never
	// sees a silently created empty database while their real data
	// still sits in the legacy location.
	legacyRoot, err := migration.DefaultLegacyRoot()
	if err != nil {
		logger.Error("legacy data detection failed", "error", err)
		log.Fatalf("debrowser: %v", err)
	}
	if filepath.Clean(legacyRoot) != filepath.Clean(cfg.Layout.Root) {
		report, err := migration.Run(cfg.Layout, legacyRoot)
		if err != nil {
			logger.Error("legacy data migration failed", "error", err)
			log.Fatalf("debrowser: legacy data migration failed: %v", err)
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

	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		logger.Error("open database failed", "error", err)
		log.Fatalf("debrowser: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.Migrate(ctx)
	cancel()
	if err != nil {
		logger.Error("database migration failed", "error", err)
		log.Fatalf("debrowser: %v", err)
	}

	appService, err := service.NewAppService(cfg, db, logger)
	if err != nil {
		logger.Error("load settings failed", "error", err)
		log.Fatalf("debrowser: %v", err)
	}

	wailsApp := app.New(cfg, appService, logger, assets)
	if err := wailsApp.Run(); err != nil {
		logger.Error("application exited with error", "error", err)
		log.Fatalf("debrowser: %v", err)
	}
}
