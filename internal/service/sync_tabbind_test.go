package service

import (
	"testing"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

func pageInfo(id, title string) cdp.TargetInfo {
	return cdp.TargetInfo{TargetID: id, Type: "page", Title: title, URL: "https://" + id + ".example/"}
}

// CDP target order is NOT tab order: a permuted target list
// must still bind correctly by title identity.
func TestBindTabOrderWithPermutedTargets(t *testing.T) {
	infos := []cdp.TargetInfo{pageInfo("tB", "Beta"), pageInfo("tA", "Alpha"), pageInfo("tC", "Gamma")}
	order, ok := bindTabOrder([]string{"Alpha", "Beta", "Gamma"}, infos)
	if !ok {
		t.Fatal("binding failed for unique titles")
	}
	want := []string{"tA", "tB", "tC"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestBindTabOrderRejectsAmbiguity(t *testing.T) {
	// Duplicate UIA titles: correspondence is unknowable.
	if _, ok := bindTabOrder([]string{"Same", "Same"},
		[]cdp.TargetInfo{pageInfo("t1", "Same"), pageInfo("t2", "Same")}); ok {
		t.Fatal("duplicate titles must not bind")
	}
	// Duplicate CDP titles.
	if _, ok := bindTabOrder([]string{"A", "B"},
		[]cdp.TargetInfo{pageInfo("t1", "A"), pageInfo("t2", "A")}); ok {
		t.Fatal("duplicate target titles must not bind")
	}
	// Count mismatch (a tab is mid-load, or a non-page target
	// was filtered differently).
	if _, ok := bindTabOrder([]string{"A"},
		[]cdp.TargetInfo{pageInfo("t1", "A"), pageInfo("t2", "B")}); ok {
		t.Fatal("count mismatch must not bind")
	}
	// A title the target list does not know.
	if _, ok := bindTabOrder([]string{"A", "Missing"},
		[]cdp.TargetInfo{pageInfo("t1", "A"), pageInfo("t2", "B")}); ok {
		t.Fatal("unknown title must not bind")
	}
	// Non-page targets are ignored on the CDP side.
	infos := []cdp.TargetInfo{pageInfo("t1", "A"), {TargetID: "bg", Type: "background_page", Title: "A"}}
	if order, ok := bindTabOrder([]string{"A"}, infos); !ok || order[0] != "t1" {
		t.Fatalf("order = %v ok = %v, want [t1] true", order, ok)
	}
}

func TestInitialBindingsSameSequence(t *testing.T) {
	mInfos := []cdp.TargetInfo{pageInfo("m2", "Beta"), pageInfo("m1", "Alpha")}
	fInfos := []cdp.TargetInfo{pageInfo("f1", "Alpha"), pageInfo("f2", "Beta")}
	b := initialBindings([]string{"Alpha", "Beta"}, mInfos, 0, []string{"Alpha", "Beta"}, fInfos, 1)
	if len(b) != 2 || b["m1"] != "f1" || b["m2"] != "f2" {
		t.Fatalf("bindings = %v, want full title-verified mapping", b)
	}
}

func TestInitialBindingsDifferentSequenceBindsCurrentOnly(t *testing.T) {
	mInfos := []cdp.TargetInfo{pageInfo("m1", "Alpha"), pageInfo("m2", "Beta")}
	fInfos := []cdp.TargetInfo{pageInfo("f1", "Alpha"), pageInfo("f2", "Other")}
	b := initialBindings([]string{"Alpha", "Beta"}, mInfos, 1, []string{"Alpha", "Other"}, fInfos, 0)
	if len(b) != 1 || b["m2"] != "f1" {
		t.Fatalf("bindings = %v, want only the two current tabs bound", b)
	}
}

func TestInitialBindingsUnverifiableIsEmpty(t *testing.T) {
	mInfos := []cdp.TargetInfo{pageInfo("m1", "Same"), pageInfo("m2", "Same")}
	fInfos := []cdp.TargetInfo{pageInfo("f1", "Same"), pageInfo("f2", "Same")}
	b := initialBindings([]string{"Same", "Same"}, mInfos, 0, []string{"Same", "Same"}, fInfos, 0)
	if len(b) != 0 {
		t.Fatalf("bindings = %v, want none when titles are ambiguous", b)
	}
}
