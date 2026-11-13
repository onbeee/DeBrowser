// Package app assembles the Wails v3 application: services, the main
// window and the background loops. It contains no business logic.
//
// Lifecycle: New creates a cancellable application context. Both
// background loops (status events, activation watcher) run under that
// context via internal/lifecycle. Run blocks until the Wails app
// exits, then cancels the context and waits for every loop to drain
// before returning — so when Run returns, no goroutine touches the
// services or the database any more, and main can close resources in
// a deterministic order.
package app

import (
	"context"
	"io/fs"
	"log/slog"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/lifecycle"
	"github.com/onbeee/DeBrowser/internal/service"
	"github.com/onbeee/DeBrowser/internal/singleinstance"
	"github.com/onbeee/DeBrowser/internal/version"
)

// App owns the Wails application and its background loops.
type App struct {
	wails  *application.App
	window *application.WebviewWindow
	cancel context.CancelFunc
	loops  []<-chan struct{}
	log    *slog.Logger
}

// New builds the Wails application around the given services. The
// assets filesystem holds the embedded frontend build (frontend/dist).
func New(cfg config.Config, appService *service.AppService, instanceService *service.InstanceService, proxyService *service.ProxyService, log *slog.Logger, assets fs.FS) *App {
	ctx, cancel := context.WithCancel(context.Background())
	a := &App{cancel: cancel, log: log}

	a.wails = application.New(application.Options{
		Name:        version.Name,
		Description: "Local-first multi-profile browser manager",
		Services: []application.Service{
			application.NewService(appService),
			application.NewService(instanceService),
			application.NewService(proxyService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Windows: application.WindowsOptions{
			// Portable mode: WebView2 must keep its user data inside the
			// application folder, never in %APPDATA%\<exe>\EBWebView.
			WebviewUserDataPath: cfg.Layout.WebView2,
		},
		OnShutdown: func() {
			// Begin draining background work as soon as shutdown
			// starts; Run completes the wait after Wails returns.
			cancel()
		},
	})

	a.window = a.wails.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            version.Name,
		Width:            1280,
		Height:           800,
		MinWidth:         1024,
		MinHeight:        640,
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/",
	})

	// Emit a fresh status snapshot on the typed event channel every
	// 10 seconds so the frontend status bar stays live without
	// polling. The loop stops with the application context.
	a.loops = append(a.loops, lifecycle.RunTicker(ctx, 10*time.Second, func() {
		a.wails.Event.Emit(service.StatusEvent, appService.GetStatus())
	}))

	// Watch for activation signals from second launches: bring the
	// existing window to the foreground instead of starting a new
	// process (see internal/singleinstance).
	activationFile := cfg.Layout.ActivationFile
	a.loops = append(a.loops, lifecycle.RunTicker(ctx, 500*time.Millisecond, func() {
		pending, err := singleinstance.ConsumeActivation(activationFile)
		if err != nil {
			log.Debug("activation signal check failed", "error", err)
			return
		}
		if pending {
			a.focusWindow()
		}
	}))

	log.Info("application assembled", "name", version.Name, "version", version.Version)
	return a
}

// focusWindow restores and focuses the main window (best effort).
func (a *App) focusWindow() {
	if a.window == nil {
		return
	}
	if a.window.IsMinimised() {
		a.window.Restore()
	}
	a.window.Show()
	a.window.Focus()
}

// Run runs the application until it exits, then drains every
// background loop before returning. The drain is UNBOUNDED by
// design: cancelling the context cannot interrupt a loop callback
// that is already running (for example a status snapshot reading
// the database), so returning on a timeout would let main close the
// database underneath a live reader. Instead, Run logs a warning
// every 5 seconds while draining and returns only when every loop
// has provably exited. Both loop callbacks are short, bounded local
// operations (a status read, a file stat), so in practice the drain
// completes in milliseconds. Only after Run returns may the caller
// close the database and other shared resources.
func (a *App) Run() error {
	err := a.wails.Run()
	a.cancel()
	lifecycle.WaitForever(5*time.Second, func() {
		a.log.Warn("background loops still draining after 5s; waiting for them to finish before shutdown continues")
	}, a.loops...)
	return err
}
