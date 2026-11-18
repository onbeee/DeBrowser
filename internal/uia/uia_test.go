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
