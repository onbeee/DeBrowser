// DeBrowser entry point. Startup order: configuration, logging, database
// (open + migrate), services, then the Wails application loop.
package main

import (
	"context"
	"embed"
	"log"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/app"
	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/service"
	"github.com/onbeee/DeBrowser/internal/version"
)

// Wails embeds the built frontend (frontend/dist) into the binary.
//
//go:embed all:frontend/dist
var assets embed.FS

func init() {
	// Register the typed status event so the binding generator produces a
	// strongly typed frontend API for it.
	application.RegisterEvent[service.Status](service.StatusEvent)
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("debrowser: %v", err)
	}

	logger, closeLog, err := logging.New(cfg)
	if err != nil {
		log.Fatalf("debrowser: %v", err)
	}
	defer func() { _ = closeLog() }()

	db, err := database.Open(cfg.DatabasePath)
	if err != nil {
		logger.Error("open database failed", "error", err)
		log.Fatalf("debrowser: %v", err)
	}

	migrateCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := db.Migrate(migrateCtx); err != nil {
		cancel()
		logger.Error("database migration failed", "error", err)
		log.Fatalf("debrowser: %v", err)
	}
	cancel()

	appService := service.NewAppService(cfg, db, logger)
	wailsApp := app.New(cfg, appService, logger, assets)

	logger.Info("starting application", "name", version.Name, "version", version.Version, "dataDir", cfg.DataDir)
	if err := wailsApp.Run(); err != nil {
		logger.Error("application exited with error", "error", err)
		log.Fatal(err)
	}
}
