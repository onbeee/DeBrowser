// Package uia provides the Windows UI Automation capabilities
// multi-instance sync (Stage 10 v2) uses for browser chrome:
// discovering what a browser window actually exposes (address
// bar, tab strip, menu button), reading which tab is selected,
// and invoking controls semantically (Invoke, SelectionItem,
// ExpandCollapse, Value patterns).
//
// Design rules, set by the Stage 10 v2 redesign:
//   - Semantic operations only. There is no SendInput or
//     coordinate-based fallback anywhere in this package: when a
//     control cannot be found unambiguously, or does not support
//     the needed pattern, the operation reports failure and the
//     caller tells the user — nothing is guessed.
//   - No window is ever activated and the real cursor never
//     moves: UIA pattern calls are not input injection.
//   - Probing never descends into the page content subtree (the
//     Document element): sync has no business reading web page
//     accessibility content, and none is collected or logged.
//   - Matching is by stable properties (AutomationId,
//     ControlType, hierarchy), never by screen coordinates.
package uia

import "errors"

// ErrUnsupported marks UI Automation on a non-Windows build, or
// a window whose UIA tree cannot be reached.
var ErrUnsupported = errors.New("uia: unsupported or unavailable")

// Control types (UIA_ControlTypeId).
const (
	ControlButton   = 50000
	ControlCheckBox = 50002
	ControlEdit     = 50004
	ControlMenu     = 50009
	ControlMenuItem = 50011
	ControlTab      = 50018
	ControlTabItem  = 50019
	ControlToolBar  = 50021
	ControlDocument = 50030
	ControlWindow   = 50032
)

// Pattern ids (UIA_*PatternId).
const (
	PatternInvoke         = 10000
	PatternValue          = 10002
	PatternExpandCollapse = 10005
	PatternSelectionItem  = 10010
	PatternToggle         = 10015
)

// ElementInfo is a snapshot of one probed chrome element.
type ElementInfo struct {
	AutomationID string
	ControlType  int32
	Name         string
	Patterns     []int32
	// Selected is only meaningful for elements supporting the
	// SelectionItem pattern (tabs).
	Selected bool
	// ExpandState is the ExpandCollapse state (0 collapsed,
	// 1 expanded, 2 partially expanded, 3 leaf), or -1 when the
	// element does not expose it.
	ExpandState int32
	// Depth is the element's depth below the window root in the
	// probed tree (root = 0).
	Depth int
}

func (e ElementInfo) HasPattern(id int32) bool {
	for _, p := range e.Patterns {
		if p == id {
			return true
		}
	}
	return false
}

// Capabilities summarizes what one browser window's chrome
// actually exposes to UI Automation. Everything here is measured
// at runtime — browsers and versions differ, and sync reports
// these as conditional support, never as guarantees.
type Capabilities struct {
	Available  bool // the window's UIA tree could be probed
	AddressBar bool // one unambiguous address bar edit control
	TabStrip   bool // selectable tab items were found
	MenuButton bool // one unambiguous menu button
	TabCount   int
	// SelectedTab is the index of the selected tab, or -1.
	SelectedTab int
	// TabTitles holds the tab items' titles in strip order,
	// aligned with SelectedTab's indexing. Titles are the only
	// identity the tab strip exposes; callers must treat the
	// mapping as reliable only when titles are unique.
	TabTitles []string
	// MenuStateKnown reports whether the menu button exposes
	// its ExpandCollapse state; MenuExpanded is that state.
	MenuStateKnown bool
	MenuExpanded   bool
	Reason         string // why Available is false, when it is
}

// MatchSpec identifies a control by stable properties. Empty
// fields are wildcards, but a match must still be unique.
type MatchSpec struct {
	ControlType  int32
	AutomationID string
	NameContains string
	// NameContainsAny matches when the name contains ANY of
	// the fragments (localized label variants).
	NameContainsAny []string
}

// MenuButtonSpec identifies a browser's main menu button across
// the label variants known Chromium builds use.
func MenuButtonSpec() MatchSpec {
	return MatchSpec{
		ControlType:     ControlButton,
		NameContainsAny: []string{"自定义及控制", "Customise and control", "Customize and control", "设置及其他"},
	}
}

// matchesSpec reports whether one element satisfies the spec.
func matchesSpec(e ElementInfo, spec MatchSpec) bool {
	if spec.ControlType != 0 && e.ControlType != spec.ControlType {
		return false
	}
	if spec.AutomationID != "" && e.AutomationID != spec.AutomationID {
		return false
	}
	if spec.NameContains != "" && !containsFold(e.Name, spec.NameContains) {
		return false
	}
	if len(spec.NameContainsAny) > 0 {
		any := false
		for _, frag := range spec.NameContainsAny {
			if containsFold(e.Name, frag) {
				any = true
				break
			}
		}
		if !any {
			return false
		}
	}
	return true
}

// FindMatch returns the index of the UNIQUE element matching the
// spec, or -1 when there is none or the match is ambiguous.
// Ambiguity is a failure, never a guess.
func FindMatch(elements []ElementInfo, spec MatchSpec) int {
	found := -1
	for i, e := range elements {
		if !matchesSpec(e, spec) {
			continue
		}
		if found >= 0 {
			return -1 // ambiguous
		}
		found = i
	}
	return found
}

// PatternForAction returns the pattern an action requires of a
// control. State actions are strict: "expand" and "collapse"
// require the ExpandCollapse pattern — Invoke is a toggle and
// can never guarantee a target state, so it is NOT accepted as
// a substitute.
func PatternForAction(el ElementInfo, action string) (int32, error) {
	var want int32
	switch action {
	case "invoke":
		want = PatternInvoke
	case "expand", "collapse":
		want = PatternExpandCollapse
	case "toggle":
		want = PatternToggle
	case "select":
		want = PatternSelectionItem
	default:
		return 0, errors.New("uia: unknown action " + action)
	}
	if !el.HasPattern(want) {
		return 0, errors.New("uia: control does not support the pattern required for " + action)
	}
	return want, nil
}

// SelectExecutable decides which element of a snapshot an
// action may run on: the UNIQUE match that also supports the
// required pattern. Anything else is an error — an operation is
// never executed on an ambiguous or incapable control.
func SelectExecutable(elements []ElementInfo, spec MatchSpec, action string) (int, error) {
	idx := FindMatch(elements, spec)
	if idx < 0 {
		return -1, errors.New("uia: no unique control matches the request")
	}
	if _, err := PatternForAction(elements[idx], action); err != nil {
		return -1, err
	}
	return idx, nil
}

func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	// Simple case-insensitive contains for ASCII; Chinese names
	// compare exactly (no case to fold).
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// AnalyzeCapabilities derives the capability summary from a
// probed chrome element snapshot. Pure and unit-tested: the
// heuristics that decide "found" live here, not in COM code.
func AnalyzeCapabilities(elements []ElementInfo) Capabilities {
	caps := Capabilities{SelectedTab: -1}
	if elements == nil {
		caps.Reason = "无法读取该浏览器窗口的界面控件信息"
		return caps
	}
	caps.Available = true

	// Address bar: an Edit whose AutomationId mentions the
	// omnibox, or whose name is the address/search prompt. Must
	// be unique.
	addr := 0
	for _, e := range elements {
		if e.ControlType != ControlEdit {
			continue
		}
		if containsFold(e.AutomationID, "omnibox") ||
			containsFold(e.AutomationID, "address-and-search") ||
			containsFold(e.Name, "地址和搜索") || containsFold(e.Name, "Address and search") {
			addr++
		}
	}
	caps.AddressBar = addr == 1

	// Tabs: TabItem elements with the SelectionItem pattern, in
	// tree order.
	tabIdx := 0
	for _, e := range elements {
		if e.ControlType == ControlTabItem && e.HasPattern(PatternSelectionItem) {
			if e.Selected {
				caps.SelectedTab = tabIdx
			}
			caps.TabTitles = append(caps.TabTitles, e.Name)
			tabIdx++
		}
	}
	caps.TabCount = tabIdx
	caps.TabStrip = tabIdx > 0

	// Menu button: a Button whose name is the browser's main
	// menu label. Must be unique.
	menu := 0
	var menuEl *ElementInfo
	for i := range elements {
		e := &elements[i]
		if e.ControlType != ControlButton {
			continue
		}
		if containsFold(e.Name, "自定义及控制") || containsFold(e.Name, "Customise and control") ||
			containsFold(e.Name, "Customize and control") || containsFold(e.Name, "设置及其他") {
			menu++
			menuEl = e
		}
	}
	caps.MenuButton = menu == 1
	if caps.MenuButton && menuEl.HasPattern(PatternExpandCollapse) && menuEl.ExpandState >= 0 {
		caps.MenuStateKnown = true
		caps.MenuExpanded = menuEl.ExpandState == 1
	}
	return caps
}

// UIOp is one semantic UI operation: an action applied to the
// unique control matching Spec. Actions: "invoke", "expand",
// "collapse", "toggle", "select". There is deliberately no
// text-setting action: sync never transfers typed text through
// UI Automation.
type UIOp struct {
	Action string
	Spec   MatchSpec
}
