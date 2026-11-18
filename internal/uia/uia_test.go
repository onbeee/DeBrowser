package uia

import "testing"

func TestFindMatchUniqueAndAmbiguous(t *testing.T) {
	elements := []ElementInfo{
		{ControlType: ControlButton, AutomationID: "menu-button", Name: "菜单"},
		{ControlType: ControlButton, AutomationID: "menu-button", Name: "菜单"},
		{ControlType: ControlEdit, AutomationID: "omnibox", Name: "地址和搜索栏"},
	}
	if got := FindMatch(elements, MatchSpec{ControlType: ControlEdit}); got != 2 {
		t.Fatalf("unique match = %d, want 2", got)
	}
	if got := FindMatch(elements, MatchSpec{ControlType: ControlButton, AutomationID: "menu-button"}); got != -1 {
		t.Fatalf("ambiguous match = %d, want -1 (never guess)", got)
	}
	if got := FindMatch(elements, MatchSpec{ControlType: ControlTabItem}); got != -1 {
		t.Fatalf("missing match = %d, want -1", got)
	}
}

func TestAnalyzeCapabilities(t *testing.T) {
	elements := []ElementInfo{
		{ControlType: ControlWindow, Name: "Chrome"},
		{ControlType: ControlEdit, AutomationID: "Omnibox", Name: "地址和搜索栏"},
		{ControlType: ControlTabItem, Patterns: []int32{PatternSelectionItem}, Selected: false},
		{ControlType: ControlTabItem, Patterns: []int32{PatternSelectionItem}, Selected: true},
		{ControlType: ControlButton, Name: "自定义及控制 Chrome"},
	}
	caps := AnalyzeCapabilities(elements)
	if !caps.Available || !caps.AddressBar || !caps.TabStrip || !caps.MenuButton {
		t.Fatalf("caps = %+v", caps)
	}
	if caps.TabCount != 2 || caps.SelectedTab != 1 {
		t.Fatalf("tabs = %d selected %d, want 2/1", caps.TabCount, caps.SelectedTab)
	}
	if caps := AnalyzeCapabilities(nil); caps.Available {
		t.Fatal("nil snapshot must be unavailable")
	}
	// Two omnibox-like edits: not counted as found.
	dup := []ElementInfo{
		{ControlType: ControlEdit, AutomationID: "Omnibox"},
		{ControlType: ControlEdit, AutomationID: "Omnibox"},
	}
	if caps := AnalyzeCapabilities(dup); caps.AddressBar {
		t.Fatal("ambiguous address bar must not count as found")
	}
}

func TestAnalyzeCapabilitiesTabTitlesAndMenuState(t *testing.T) {
	els := []ElementInfo{
		{ControlType: ControlTabItem, Name: "文档一", Patterns: []int32{PatternSelectionItem}, Selected: false, ExpandState: -1},
		{ControlType: ControlTabItem, Name: "文档二", Patterns: []int32{PatternSelectionItem}, Selected: true, ExpandState: -1},
		{ControlType: ControlButton, Name: "自定义及控制 Chrome", Patterns: []int32{PatternInvoke, PatternExpandCollapse}, ExpandState: 1},
	}
	caps := AnalyzeCapabilities(els)
	if len(caps.TabTitles) != 2 || caps.TabTitles[0] != "文档一" || caps.TabTitles[1] != "文档二" {
		t.Fatalf("TabTitles = %v, want both titles in strip order", caps.TabTitles)
	}
	if caps.SelectedTab != 1 {
		t.Fatalf("SelectedTab = %d, want 1 (aligned with TabTitles)", caps.SelectedTab)
	}
	if !caps.MenuStateKnown || !caps.MenuExpanded {
		t.Fatalf("menu state = known %v expanded %v, want known+expanded", caps.MenuStateKnown, caps.MenuExpanded)
	}
	// Ambiguous menu buttons: state must be unknown, never guessed.
	els = append(els, ElementInfo{ControlType: ControlButton, Name: "自定义及控制", Patterns: []int32{PatternExpandCollapse}, ExpandState: 0})
	caps = AnalyzeCapabilities(els)
	if caps.MenuButton || caps.MenuStateKnown {
		t.Fatalf("ambiguous menu: MenuButton=%v MenuStateKnown=%v, want both false", caps.MenuButton, caps.MenuStateKnown)
	}
}

func TestFindMatchNameContainsAny(t *testing.T) {
	els := []ElementInfo{
		{ControlType: ControlButton, Name: "Customize and control Google Chrome"},
		{ControlType: ControlButton, Name: "返回"},
	}
	if idx := FindMatch(els, MenuButtonSpec()); idx != 0 {
		t.Fatalf("FindMatch(MenuButtonSpec) = %d, want 0", idx)
	}
	els = append(els, ElementInfo{ControlType: ControlButton, Name: "自定义及控制"})
	if idx := FindMatch(els, MenuButtonSpec()); idx != -1 {
		t.Fatalf("FindMatch with two menu buttons = %d, want -1 (ambiguous)", idx)
	}
}

func TestPatternForActionStrictStates(t *testing.T) {
	invokeOnly := ElementInfo{Patterns: []int32{PatternInvoke}}
	withEC := ElementInfo{Patterns: []int32{PatternInvoke, PatternExpandCollapse}}
	// expand/collapse must never fall back to Invoke (a toggle
	// cannot guarantee a target state).
	if _, err := PatternForAction(invokeOnly, "expand"); err == nil {
		t.Fatal("expand on an Invoke-only control must fail")
	}
	if _, err := PatternForAction(invokeOnly, "collapse"); err == nil {
		t.Fatal("collapse on an Invoke-only control must fail")
	}
	if p, err := PatternForAction(withEC, "expand"); err != nil || p != PatternExpandCollapse {
		t.Fatalf("expand with ExpandCollapse = %v, %v", p, err)
	}
	if p, err := PatternForAction(withEC, "collapse"); err != nil || p != PatternExpandCollapse {
		t.Fatalf("collapse with ExpandCollapse = %v, %v", p, err)
	}
	if p, err := PatternForAction(invokeOnly, "invoke"); err != nil || p != PatternInvoke {
		t.Fatalf("invoke = %v, %v", p, err)
	}
}

func TestSelectExecutable(t *testing.T) {
	spec := MatchSpec{ControlType: ControlButton, NameContains: "菜单"}
	one := []ElementInfo{
		{ControlType: ControlButton, Name: "菜单", Patterns: []int32{PatternExpandCollapse}},
		{ControlType: ControlButton, Name: "返回", Patterns: []int32{PatternInvoke}},
	}
	if idx, err := SelectExecutable(one, spec, "expand"); err != nil || idx != 0 {
		t.Fatalf("SelectExecutable = %d, %v; want 0, nil", idx, err)
	}
	// Ambiguous: refuse.
	two := append(append([]ElementInfo{}, one...),
		ElementInfo{ControlType: ControlButton, Name: "菜单二", Patterns: []int32{PatternExpandCollapse}})
	if _, err := SelectExecutable(two, spec, "expand"); err == nil {
		t.Fatal("ambiguous match must be refused")
	}
	// Unique but incapable: refuse.
	incapable := []ElementInfo{
		{ControlType: ControlButton, Name: "菜单", Patterns: []int32{PatternInvoke}},
	}
	if _, err := SelectExecutable(incapable, spec, "expand"); err == nil {
		t.Fatal("control without ExpandCollapse must be refused for expand")
	}
	// Absent: refuse.
	if _, err := SelectExecutable(one, MatchSpec{NameContains: "不存在"}, "invoke"); err == nil {
		t.Fatal("missing control must be refused")
	}
}
