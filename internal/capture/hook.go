package capture

import "time"

// Zone classifies where an observed interaction happened.
type Zone int

const (
	// ZoneUnknown: the source cannot be attributed — never
	// forwarded anywhere.
	ZoneUnknown Zone = iota
	// ZonePage: the page content area — handled by the CDP
	// channel (the page script is the source of truth there;
	// hook copies of page events are discarded).
	ZonePage
	// ZoneNativeUI: the browser's own UI (address bar, tab
	// strip, menus) — only operations whose effect is later
	// confirmed through CDP (a committed navigation) are
	// synchronized in the first version.
	ZoneNativeUI
)

// RawHookEvent is one low-level hook observation, reduced to
// the minimum the classifier needs. It never carries typed
// text or page content.
type RawHookEvent struct {
	IsKey    bool
	KeyDown  bool
	VKCode   uint32
	MouseMsg uint32 // WM_* for mouse events
	X, Y     int32  // screen coordinates (mouse only)
	// HWND is the window the event is attributed to: the
	// window under the cursor for mouse events, the
	// foreground window for keyboard events.
	HWND uintptr
	At   time.Time
}

// HookObserver observes low-level keyboard/mouse activity
// system-wide but only REPORTS events attributable to one
// verified master process. Implementations must keep the hook
// callback minimal: attribute, enqueue non-blockingly, pass
// the event on. Observation only — the hook never blocks or
// rewrites user input.
type HookObserver interface {
	Start(pid int) error
	Stop()
	Events() <-chan RawHookEvent
}

// Classify attributes a hook event: an event whose window
// belongs to the master process is native-UI unless a page
// script event of the same flavour just arrived (then the
// script owns it and the hook copy is the page zone echo).
// Anything else is unknown and dropped by callers.
func Classify(ev RawHookEvent, masterPID int, ownerPID func(hwnd uintptr) int, pageEcho bool) Zone {
	if ev.HWND == 0 || masterPID <= 0 {
		return ZoneUnknown
	}
	if ownerPID(ev.HWND) != masterPID {
		return ZoneUnknown
	}
	if pageEcho {
		return ZonePage
	}
	return ZoneNativeUI
}

// NewHookObserver returns the platform observer (a no-op
// observer off Windows).
func NewHookObserver() HookObserver { return newHookObserver() }
