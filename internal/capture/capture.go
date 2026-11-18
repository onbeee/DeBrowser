// Package capture observes the user's input on the MASTER
// browser only, through Windows low-level hooks, for
// multi-instance sync (Stage 10 v2).
//
// It is observation-only by design: the package exposes no way
// to inject input anywhere. The previous design's SendInput
// mirroring — which moved the user's real cursor and fought over
// foreground focus — is gone entirely. Followers receive input
// through CDP (page content) or semantic operations (browser
// UI), never through synthesized system input.
//
// The handler is invoked from the hook callback and MUST return
// immediately; handlers hand events to bounded queues.
package capture

import "errors"

// ErrUnsupported marks capture on a non-Windows build.
var ErrUnsupported = errors.New("capture: unsupported platform")

type EventKind string

const (
	EventMouseMove  EventKind = "mouseMove"
	EventMouseDown  EventKind = "mouseDown"
	EventMouseUp    EventKind = "mouseUp"
	EventMouseWheel EventKind = "mouseWheel"
	EventKeyDown    EventKind = "keyDown"
	EventKeyUp      EventKind = "keyUp"
)

// Event is one observed input event in screen coordinates.
type Event struct {
	Kind EventKind
	// Mouse: screen position and button state.
	X, Y        int32
	Button      string // "left", "middle", "right" for down/up
	ButtonsMask int    // bitmask after the event: 1 left, 2 right, 4 middle
	DeltaX      int32  // wheel deltas (wheel only)
	DeltaY      int32
	// Keyboard: virtual key code.
	VK uint32
}

// Handler consumes observed events. It runs on the hook thread
// and must never block.
type Handler func(Event)
