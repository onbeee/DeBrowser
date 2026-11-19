package service

import (
	"encoding/json"
	"strings"
)

// Stage 10 v3 page-event model. Events are produced by the
// injected observer script (sync_capture_script.js) inside the
// master browser's pages and delivered through a CDP Runtime
// binding. They describe page input in CSS viewport coordinates
// and real browser key terms — never Windows desktop coordinates
// or virtual-key guesses.
//
// Privacy: a text event carries only the increment the user just
// committed (an input event's data or an IME composition result).
// Text lives in the sync session's memory only while it is being
// dispatched; it is never logged, persisted or stored. No event
// carries a field's full value, page text, cookies, storage or
// history. The URL in a visibility event is navigation
// information, likewise session-memory only.

// PageEvent is one observed page event.
type PageEvent struct {
	Kind string `json:"kind"`

	// visibility
	State string `json:"state,omitempty"` // "visible" | "hidden"
	Href  string `json:"href,omitempty"`

	// pointer / wheel (CSS viewport pixels)
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	Button  int     `json:"button,omitempty"`
	Buttons int     `json:"buttons,omitempty"`
	Detail  int     `json:"detail,omitempty"`
	DeltaX  float64 `json:"deltaX,omitempty"`
	DeltaY  float64 `json:"deltaY,omitempty"`

	// keyboard (browser terms)
	Key       string `json:"key,omitempty"`
	Code      string `json:"code,omitempty"`
	Location  int    `json:"location,omitempty"`
	Repeat    bool   `json:"repeat,omitempty"`
	Composing bool   `json:"composing,omitempty"`

	// modifiers
	Alt   bool `json:"alt,omitempty"`
	Ctrl  bool `json:"ctrl,omitempty"`
	Meta  bool `json:"meta,omitempty"`
	Shift bool `json:"shift,omitempty"`

	// text increment (kind == "text")
	Text string `json:"text,omitempty"`
	// Source says how a text increment was produced: "input"
	// (typed or dropped text), "paste" (insertFromPaste) or
	// "composition" (an IME composition's final commit). It drives
	// the composition-twin dedup and is never the text itself.
	Source string `json:"source,omitempty"`
}

// IsPasteShortcut reports whether a key event is a paste
// shortcut (Ctrl/Cmd+V or Shift+Insert). Paste follows ONE path
// in this design — the insertFromPaste text increment — so the
// shortcut itself is never dispatched to followers: dispatching
// both would paste twice. The observer script already withholds
// these; the check is applied again on the dispatch side so the
// single-path rule holds even if a report slips through. A paste
// that produces no input event (for example into a canvas) is an
// explicitly unsupported paste target, never guessed at.
func (e PageEvent) IsPasteShortcut() bool {
	if e.Kind != "keydown" && e.Kind != "keyup" {
		return false
	}
	if (e.Ctrl || e.Meta) && (e.Code == "KeyV" || strings.EqualFold(e.Key, "v")) {
		return true
	}
	return e.Shift && !e.Ctrl && !e.Meta && !e.Alt &&
		(e.Code == "Insert" || e.Key == "Insert")
}

// cdpModifiers folds the modifier booleans into the CDP Input
// modifier bitmask (Alt=1, Control=2, Meta/Command=4, Shift=8).
func (e PageEvent) cdpModifiers() int {
	m := 0
	if e.Alt {
		m |= 1
	}
	if e.Ctrl {
		m |= 2
	}
	if e.Meta {
		m |= 4
	}
	if e.Shift {
		m |= 8
	}
	return m
}

// bindingCalledParams is the Runtime.bindingCalled payload.
type bindingCalledParams struct {
	Name               string `json:"name"`
	Payload            string `json:"payload"`
	ExecutionContextID int    `json:"executionContextId"`
}

// executionContextCreatedParams is the
// Runtime.executionContextCreated payload (subset).
type executionContextCreatedParams struct {
	Context struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		AuxData struct {
			FrameID   string `json:"frameId"`
			IsDefault bool   `json:"isDefault"`
		} `json:"auxData"`
	} `json:"context"`
}

// frameNavigatedParams is the Page.frameNavigated payload
// (subset). A frame without a parent ID is the main frame.
type frameNavigatedParams struct {
	Frame struct {
		ID       string `json:"id"`
		ParentID string `json:"parentId,omitempty"`
		LoaderID string `json:"loaderId"`
		URL      string `json:"url"`
	} `json:"frame"`
}

// targetCreatedParams / targetDestroyedParams are the Target
// event payloads (subsets).
type targetCreatedParams struct {
	TargetInfo targetInfoJSON `json:"targetInfo"`
}

type targetDestroyedParams struct {
	TargetID string `json:"targetId"`
}

type targetInfoJSON struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	OpenerID string `json:"openerId,omitempty"`
}

func (t targetInfoJSON) url() string { return t.URL }

// parsePageEvent decodes one binding payload into a PageEvent.
func parsePageEvent(payload string) (PageEvent, bool) {
	var ev PageEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil || ev.Kind == "" {
		return PageEvent{}, false
	}
	return ev, true
}
