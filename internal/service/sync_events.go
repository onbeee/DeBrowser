package service

// PageEvent is one interaction observed on the MASTER
// instance's active page by the injected observer script. It
// travels only inside the sync session's memory: never logged,
// never persisted. Text payloads (input increments, paste
// increments, IME results) exist only in the Text field of the
// relevant kinds.
type PageEvent struct {
	Kind      string  `json:"kind"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Button    int     `json:"button"`
	Buttons   int     `json:"buttons"`
	DeltaX    float64 `json:"deltaX"`
	DeltaY    float64 `json:"deltaY"`
	Key       string  `json:"key"`
	Code      string  `json:"code"`
	Location  int     `json:"location"`
	Repeat    bool    `json:"repeat"`
	Ctrl      bool    `json:"ctrl"`
	Alt       bool    `json:"alt"`
	Shift     bool    `json:"shift"`
	Meta      bool    `json:"meta"`
	Composing bool    `json:"composing"`
	Text      string  `json:"text"`
	Visible   bool    `json:"visible"`

	// Source is set server-side: the CDP target the event was
	// observed on. The script never sets it.
	Source string `json:"-"`
}

// IsPasteShortcut reports whether a key event is the paste
// shortcut, whose text must arrive exactly once — as the
// paste increment — never additionally as keystrokes.
func (e PageEvent) IsPasteShortcut() bool {
	if (e.Ctrl || e.Meta) && e.Code == "KeyV" {
		return true
	}
	return e.Shift && e.Code == "Insert"
}

// IsKeyRelease reports keyup events (critical: never dropped).
func (e PageEvent) IsKeyRelease() bool { return e.Kind == "keyup" }

// IsPointerRelease reports pointerup events (critical).
func (e PageEvent) IsPointerRelease() bool { return e.Kind == "pointerup" }

// IsCritical reports events whose loss would corrupt the
// follower's input state (releases) or the session's document
// flow (text, navigation, target lifecycle).
func (e PageEvent) IsCritical() bool {
	switch e.Kind {
	case "keyup", "pointerup", "input", "insertfrompaste", "compositionend",
		"navigate", "reload", "historyback", "historyforward",
		"targetcreated", "targetclosed", "targetactivated":
		return true
	}
	return false
}

// Modifiers returns the CDP modifier bitmask.
func (e PageEvent) Modifiers() int {
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
