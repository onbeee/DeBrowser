package service

import (
	"log/slog"
	"sync"

	"github.com/onbeee/DeBrowser/internal/initialize"
	"github.com/onbeee/DeBrowser/internal/paths"
)

// WelcomeService backs the Stage 5A first-launch window. It is the
// ONLY service registered in welcome mode: no AppService,
// InstanceService or ProxyService exists before initialization, so no
// business binding can touch a database or vault that does not exist
// yet.
//
// The "import data" entry deliberately has no backend method in this
// stage: choosing it is a pure frontend view change that shows the
// Stage 5B notice and creates nothing on disk.
type WelcomeService struct {
	layout  paths.Layout
	log     *slog.Logger
	mu      sync.Mutex
	done    bool
	onReady func()
}

// NewWelcomeService creates the welcome service for the resolved
// (but not yet created) portable layout. onReady is invoked once
// after a successful initialization (the application layer uses it
// to start the formal process and close the welcome window); it is
// a constructor parameter, not a method, so it never becomes a
// frontend binding.
func NewWelcomeService(layout paths.Layout, log *slog.Logger, onReady func()) *WelcomeService {
	return &WelcomeService{layout: layout, log: log, onReady: onReady}
}

// StartNewUser runs the new-user initialization. It returns an empty
// string on success — the welcome window then closes and the formal
// application starts — or a Chinese, user-facing error message; after
// a failure the installation has been cleaned back to exe-only and
// the user may simply try again.
func (s *WelcomeService) StartNewUser() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return ""
	}
	if err := initialize.NewUser(s.layout, s.log); err != nil {
		return err.Error()
	}
	s.done = true
	if s.onReady != nil {
		s.onReady()
	}
	return ""
}
