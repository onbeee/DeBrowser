// Package app assembles the Wails v3 application: services, the main
// window and the periodic status event. It contains no business logic.
package app

import (
	"io/fs"
	"log/slog"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/service"
	"github.com/onbeee/DeBrowser/internal/version"
)

// New builds the Wails application around the given AppService. The assets
// filesystem holds the embedded frontend build (frontend/dist).
func New(cfg config.Config, appService *service.AppService, log *slog.Logger, assets fs.FS) *application.App {
	wailsApp := application.New(application.Options{
		Name:        version.Name,
		Description: "Local-first multi-profile browser manager",
		Services: []application.Service{
			application.NewService(appService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Windows: application.WindowsOptions{
			// Portable mode: WebView2 must keep its user data inside the
			// application folder, never in %APPDATA%\<exe>\EBWebView.
			WebviewUserDataPath: cfg.Layout.WebView2,
		},
	})

	wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            version.Name,
		Width:            1280,
		Height:           800,
		MinWidth:         1024,
		MinHeight:        640,
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/",
	})

	// Emit a fresh status snapshot on the typed event channel every
	// 10 seconds so the frontend status bar stays live without polling.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			wailsApp.Event.Emit(service.StatusEvent, appService.GetStatus())
		}
	}()

	log.Info("application assembled", "name", version.Name, "version", version.Version)
	return wailsApp
}
