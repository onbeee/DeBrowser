package app

import (
	"context"
	"io/fs"
	"log/slog"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/installstate"
	"github.com/onbeee/DeBrowser/internal/lifecycle"
	"github.com/onbeee/DeBrowser/internal/service"
	"github.com/onbeee/DeBrowser/internal/singleinstance"
	"github.com/onbeee/DeBrowser/internal/version"
)

// WelcomeApp is the Stage 5A first-launch window. It registers only
// the WelcomeService and runs entirely out of the temporary welcome
// workspace: its WebView2 user data lives in the system temp
// directory, never in the formal webview2-data folder, and no formal
// directory, database or vault exists while it is shown.
type WelcomeApp struct {
	wails  *application.App
	window *application.WebviewWindow
	cancel context.CancelFunc
	loops  []<-chan struct{}
	log    *slog.Logger
}

// NewWelcome builds the welcome application. ws is the temporary
// workspace (see installstate.TempWorkspace) holding this mode's
// WebView2 data and activation file.
func NewWelcome(cfg config.Config, welcomeService *service.WelcomeService, ws installstate.Workspace, log *slog.Logger, assets fs.FS) *WelcomeApp {
	ctx, cancel := context.WithCancel(context.Background())
	a := &WelcomeApp{cancel: cancel, log: log}

	a.wails = application.New(application.Options{
		Name:        version.Name,
		Description: "Local-first multi-profile browser manager",
		Services: []application.Service{
			application.NewService(welcomeService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Windows: application.WindowsOptions{
			// Welcome mode must not create the formal WebView2 data
			// directory: its runtime data stays in the temp workspace
			// and is removed when the welcome process exits.
			WebviewUserDataPath: ws.WebView2,
		},
		OnShutdown: func() {
			cancel()
		},
	})

	a.window = a.wails.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            version.Name,
		Width:            760,
		Height:           560,
		MinWidth:         640,
		MinHeight:        480,
		BackgroundColour: application.NewRGB(15, 17, 21),
		URL:              "/welcome.html",
	})

	// A second launch while the welcome window is open signals this
	// file (through the temp workspace lock); focus the window
	// instead of opening a second welcome flow.
	activationFile := ws.ActivationFile
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

	log.Info("welcome application assembled", "name", version.Name, "version", version.Version)
	return a
}

func (a *WelcomeApp) focusWindow() {
	if a.window == nil {
		return
	}
	if a.window.IsMinimised() {
		a.window.Restore()
	}
	a.window.Show()
	a.window.Focus()
}

// Quit closes the welcome application programmatically, used after a
// successful initialization has started the formal process.
func (a *WelcomeApp) Quit() {
	a.wails.Quit()
}

// Run runs the welcome application until it exits, then drains its
// background loop before returning (same contract as App.Run).
func (a *WelcomeApp) Run() error {
	err := a.wails.Run()
	a.cancel()
	lifecycle.WaitForever(5*time.Second, func() {
		a.log.Warn("background loops still draining after 5s; waiting for them to finish before shutdown continues")
	}, a.loops...)
	return err
}
