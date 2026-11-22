package store

import (
	"context"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

// TestSetCustomRuleOrderSetValidation pins the reorder set
// discipline: the id list must be exactly the current rule set
// (no duplicates, none missing, none unknown), validated inside
// the update transaction — a rejected reorder leaves every
// sort_order untouched.
func TestSetCustomRuleOrderSetValidation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mk := func(value string, order int) model.ProxyCustomRule {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.CreateCustomRule(ctx, model.ProxyCustomRule{
			ID: id, RuleType: model.CustomRuleDomainSuffix, Value: value,
			Policy: model.CustomRulePolicyProxy, Enabled: true, SortOrder: order,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a := mk("a.com", 0)
	b := mk("b.com", 1)
	c := mk("c.com", 2)

	orders := func() map[string]int {
		rules, err := s.ListCustomRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, r := range rules {
			out[r.ID] = r.SortOrder
		}
		return out
	}
	wantOriginal := map[string]int{a.ID: 0, b.ID: 1, c.ID: 2}

	bad := map[string][]string{
		"duplicate id":  {a.ID, a.ID, b.ID},
		"missing one":   {a.ID, b.ID},
		"unknown id":    {a.ID, b.ID, "no-such-rule"},
		"extra id":      {a.ID, b.ID, c.ID, "no-such-rule"},
		"unknown swap":  {a.ID, b.ID, "no-such-rule"},
		"empty":         {},
		"partial write": {c.ID, b.ID, "no-such-rule"}, // would move c,b before failing
	}
	for name, ids := range bad {
		if err := s.SetCustomRuleOrder(ctx, ids); err == nil {
			t.Fatalf("%s: want an error", name)
		}
		got := orders()
		for id, want := range wantOriginal {
			if got[id] != want {
				t.Fatalf("%s: sort_order of %s = %d, want %d (a rejected reorder must write nothing)", name, id, got[id], want)
			}
		}
	}

	// A complete, exact reorder applies.
	if err := s.SetCustomRuleOrder(ctx, []string{c.ID, a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	got := orders()
	if got[c.ID] != 0 || got[a.ID] != 1 || got[b.ID] != 2 {
		t.Fatalf("orders after reorder = %v, want c=0 a=1 b=2", got)
	}
}
