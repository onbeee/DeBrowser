package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---- planInitialMapping (pure pairing rules) ----

func TestPlanInitialMappingSingleTab(t *testing.T) {
	pairs, toCreate, err := planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: "https://a.test/"}},
		[]pageTargetInfo{{ID: "F1", URL: "https://a.test/"}},
	)
	if err != nil || len(toCreate) != 0 || pairs["M1"] != "F1" {
		t.Fatalf("pairs=%v create=%v err=%v", pairs, toCreate, err)
	}
	// A tab whose URL is not yet known on the master side
	// still pairs with the follower's ONLY tab, by
	// elimination — never by guessing among several.
	pairs, _, err = planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: ""}},
		[]pageTargetInfo{{ID: "F1", URL: "chrome://newtab/"}},
	)
	if err != nil || pairs["M1"] != "F1" {
		t.Fatalf("elimination pairing: pairs=%v err=%v", pairs, err)
	}
}

func TestPlanInitialMappingTwoTabs(t *testing.T) {
	pairs, toCreate, err := planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: "https://a.test/"}, {ID: "M2", URL: "https://b.test/"}},
		[]pageTargetInfo{{ID: "F1", URL: "https://a.test/"}, {ID: "F2", URL: "https://b.test/"}},
	)
	if err != nil || len(toCreate) != 0 {
		t.Fatalf("create=%v err=%v", toCreate, err)
	}
	if pairs["M1"] != "F1" || pairs["M2"] != "F2" {
		t.Fatalf("pairs=%v", pairs)
	}
}

func TestPlanInitialMappingDuplicateURLsPairDistinctly(t *testing.T) {
	pairs, toCreate, err := planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: "https://same.test/"}, {ID: "M2", URL: "https://same.test/"}},
		[]pageTargetInfo{{ID: "F1", URL: "https://same.test/"}, {ID: "F2", URL: "https://same.test/"}},
	)
	if err != nil || len(toCreate) != 0 {
		t.Fatalf("create=%v err=%v", toCreate, err)
	}
	if pairs["M1"] == "" || pairs["M2"] == "" || pairs["M1"] == pairs["M2"] {
		t.Fatalf("same-URL tabs must pair with DISTINCT follower tabs: %v", pairs)
	}
	if pairs["M1"] != "F1" || pairs["M2"] != "F2" {
		t.Fatalf("same-URL tabs must pair by ordinal position: %v", pairs)
	}
}

func TestPlanInitialMappingUnmatchedKnownURLCreates(t *testing.T) {
	pairs, toCreate, err := planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: "https://a.test/"}, {ID: "M2", URL: "https://b.test/"}},
		[]pageTargetInfo{{ID: "F1", URL: "https://a.test/"}},
	)
	if err != nil || pairs["M1"] != "F1" || len(toCreate) != 1 || toCreate[0].ID != "M2" {
		t.Fatalf("pairs=%v create=%v err=%v", pairs, toCreate, err)
	}
	// A follower tab on an unrelated URL is NOT paired with
	// a master tab on a different known URL.
	pairs, toCreate, err = planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: "https://a.test/"}},
		[]pageTargetInfo{{ID: "F1", URL: "https://other.test/"}},
	)
	if err != nil || len(pairs) != 0 || len(toCreate) != 1 {
		t.Fatalf("mismatched URLs must not pair: pairs=%v create=%v err=%v", pairs, toCreate, err)
	}
}

func TestPlanInitialMappingAmbiguousFails(t *testing.T) {
	// Several master tabs with unknown URLs and several
	// follower tabs: no reliable correspondence exists.
	if _, _, err := planInitialMapping(
		[]pageTargetInfo{{ID: "M1"}, {ID: "M2"}},
		[]pageTargetInfo{{ID: "F1"}, {ID: "F2"}},
	); !errors.Is(err, errInitialMapping) {
		t.Fatalf("err = %v, want errInitialMapping", err)
	}
	// An extra master tab with an unknown URL and no
	// follower tab left to pair with: cannot create it
	// either (no URL), so pairing must fail, not guess.
	if _, _, err := planInitialMapping(
		[]pageTargetInfo{{ID: "M1", URL: "https://a.test/"}, {ID: "M2"}},
		[]pageTargetInfo{{ID: "F1", URL: "https://a.test/"}},
	); !errors.Is(err, errInitialMapping) {
		t.Fatalf("err = %v, want errInitialMapping", err)
	}
}

// ---- initTargets against a fake follower browser ----

// initWorker builds a worker whose follower browser already
// shows followerTargets, runs the initial mapping against
// master, and starts the dispatch loop.
func initWorker(t *testing.T, f *fakeBrowserCDP, master []pageTargetInfo, active string) *followerWorker {
	t.Helper()
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(w.close)
	if err := w.initTargets(ctx, master, active); err != nil {
		t.Fatalf("initTargets: %v", err)
	}
	w.start(ctx)
	return w
}

func TestInitTargetsReusesSingleInitialTab(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.targets = []fakeTargetInfo{{ID: "F1", URL: "https://a.test/"}}
	w := initWorker(t, f,
		[]pageTargetInfo{{ID: "M1", URL: "https://a.test/"}}, "M1")
	if ft, ok := w.mappedTarget("M1"); !ok || ft != "F1" {
		t.Fatalf("mapping = %q,%v — the initial tab must be REUSED", ft, ok)
	}
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("creates = %d, want 0 (no extra tab for the initial pair)", n)
	}
	if got := f.activatedTargetIDs(); len(got) != 1 || got[0] != "F1" {
		t.Fatalf("activated = %v, want [F1] (the active counterpart)", got)
	}
}

func TestInitTargetsReusesTwoTabsActiveIsSecond(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.targets = []fakeTargetInfo{
		{ID: "F1", URL: "https://a.test/"},
		{ID: "F2", URL: "https://b.test/"},
	}
	w := initWorker(t, f, []pageTargetInfo{
		{ID: "M1", URL: "https://a.test/"},
		{ID: "M2", URL: "https://b.test/"},
	}, "M2")
	if ft, _ := w.mappedTarget("M1"); ft != "F1" {
		t.Fatalf("M1 -> %q, want F1", ft)
	}
	if ft, _ := w.mappedTarget("M2"); ft != "F2" {
		t.Fatalf("M2 -> %q, want F2", ft)
	}
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("creates = %d, want 0", n)
	}
	// The active master tab is the SECOND one; its
	// counterpart is the one activated on the follower.
	if got := f.activatedTargetIDs(); len(got) != 1 || got[0] != "F2" {
		t.Fatalf("activated = %v, want [F2]", got)
	}
}

func TestInitTargetsDuplicateURLsGetDistinctCounterparts(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.targets = []fakeTargetInfo{
		{ID: "F1", URL: "https://same.test/"},
		{ID: "F2", URL: "https://same.test/"},
	}
	w := initWorker(t, f, []pageTargetInfo{
		{ID: "M1", URL: "https://same.test/"},
		{ID: "M2", URL: "https://same.test/"},
	}, "M1")
	m1, _ := w.mappedTarget("M1")
	m2, _ := w.mappedTarget("M2")
	if m1 == "" || m2 == "" || m1 == m2 {
		t.Fatalf("duplicate-URL mappings must be distinct: M1->%q M2->%q", m1, m2)
	}
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("creates = %d, want 0", n)
	}
}

func TestInitTargetsCreatesOnlyForUnmatched(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.targets = []fakeTargetInfo{{ID: "F1", URL: "https://a.test/"}}
	w := initWorker(t, f, []pageTargetInfo{
		{ID: "M1", URL: "https://a.test/"},
		{ID: "M2", URL: "https://b.test/"},
	}, "M1")
	if ft, _ := w.mappedTarget("M1"); ft != "F1" {
		t.Fatalf("M1 -> %q, want reused F1", ft)
	}
	if ft, _ := w.mappedTarget("M2"); ft != "FT-1" {
		t.Fatalf("M2 -> %q, want the one created tab FT-1", ft)
	}
	if n := f.creates.Load(); n != 1 {
		t.Fatalf("creates = %d, want exactly 1", n)
	}
}

func TestInitTargetsFailureCleansCreatedTabs(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.failCreateAt = 2 // the second created tab fails
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	err := w.initTargets(context.Background(), []pageTargetInfo{
		{ID: "M1", URL: "https://a.test/"},
		{ID: "M2", URL: "https://b.test/"},
	}, "M1")
	if err == nil {
		t.Fatal("second creation failing must fail initialization")
	}
	// The first (successfully created) tab is closed again:
	// a failed initialization leaves no orphan tab behind.
	closed := f.closedTargetIDs()
	if len(closed) != 1 || closed[0] != "FT-1" {
		t.Fatalf("closed after failed init = %v, want [FT-1]", closed)
	}
	if got := w.mappedMasterTargets(); len(got) != 0 {
		t.Fatalf("mappings left after failed init: %v", got)
	}
}

func TestInitTargetsAmbiguousFailsWithoutCreating(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.targets = []fakeTargetInfo{{ID: "F1"}, {ID: "F2"}}
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	err := w.initTargets(context.Background(), []pageTargetInfo{{ID: "M1"}, {ID: "M2"}}, "M1")
	if !errors.Is(err, errInitialMapping) {
		t.Fatalf("err = %v, want errInitialMapping", err)
	}
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("creates = %d, want 0 (no guessed tabs)", n)
	}
}

// ---- Closing tabs that existed before the session ----

func TestClosePreExistingTabsActiveAndNot(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.targets = []fakeTargetInfo{
		{ID: "F1", URL: "https://a.test/"},
		{ID: "F2", URL: "https://b.test/"},
	}
	w := initWorker(t, f, []pageTargetInfo{
		{ID: "M1", URL: "https://a.test/"},
		{ID: "M2", URL: "https://b.test/"},
	}, "M2") // M2 is the active one

	// Close the NON-active pre-existing tab first.
	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"})
	waitFor(t, "non-active pre-existing tab closed", func() bool {
		closed := f.closedTargetIDs()
		return len(closed) == 1 && closed[0] == "F1"
	})
	if _, ok := w.mappedTarget("M1"); ok {
		t.Fatal("M1 mapping must be dropped after its close")
	}
	if ft, ok := w.mappedTarget("M2"); !ok || ft != "F2" {
		t.Fatal("M2 mapping must survive M1's close")
	}

	// Then the active pre-existing tab.
	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M2"})
	waitFor(t, "active pre-existing tab closed", func() bool {
		closed := f.closedTargetIDs()
		return len(closed) == 2 && closed[1] == "F2"
	})
	if got := w.mappedMasterTargets(); len(got) != 0 {
		t.Fatalf("mappings left: %v", got)
	}
}

func TestConsecutiveAndDuplicateCloses(t *testing.T) {
	f := newFakeBrowserCDP(t)
	w := startedWorker(t, f) // M1->FT-1, M2->FT-2 (created during the session's life)

	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"})
	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"}) // duplicate: must be a no-op
	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M2"})
	waitFor(t, "both tabs closed exactly once", func() bool {
		return f.closes.Load() == 2
	})
	time.Sleep(120 * time.Millisecond)
	if n := f.closes.Load(); n != 2 {
		t.Fatalf("closes = %d, want exactly 2 (duplicate destroyed event must not close another tab)", n)
	}
	closed := f.closedTargetIDs()
	if len(closed) != 2 || closed[0] != "FT-1" || closed[1] != "FT-2" {
		t.Fatalf("closed = %v, want [FT-1 FT-2]", closed)
	}
}

func TestOneFollowersCloseFailureIsIsolated(t *testing.T) {
	f1 := newFakeBrowserCDP(t)
	w1 := startedWorker(t, f1)
	f2 := newFakeBrowserCDP(t)
	w2 := startedWorker(t, f2)

	w1.markFailed("跟随实例的调试连接已断开（模拟）")
	// The engine fans the closure out to every follower; the
	// failed one cannot take it, the healthy one still does.
	if w1.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"}) {
		t.Fatal("a failed follower must not accept events")
	}
	w2.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"})
	waitFor(t, "healthy follower closed its tab", func() bool {
		return f2.closes.Load() == 1
	})
	if n := f1.closes.Load(); n != 0 {
		t.Fatalf("failed follower closes = %d, want 0", n)
	}
}

// ---- The last tab: destruction racing disconnect/teardown ----

// A targetclosed that is queued but not yet dispatched when
// the worker is closed (the master's connection died and the
// session is being torn down) must STILL be executed: the
// worker's close drains queued closures before its
// connection closes.
func TestQueuedCloseSurvivesWorkerClose(t *testing.T) {
	f := newFakeBrowserCDP(t)
	w := startedWorker(t, f)

	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"})
	w.close() // returns only after the dispatch loop ended
	if n := f.closes.Load(); n != 1 {
		t.Fatalf("closes = %d, want 1 (queued closure drained on close)", n)
	}
	if closed := f.closedTargetIDs(); len(closed) != 1 || closed[0] != "FT-1" {
		t.Fatalf("closed = %v, want [FT-1]", closed)
	}
	if _, ok := w.mappedTarget("M1"); ok {
		t.Fatal("M1 mapping must be gone after the drained close")
	}
	if _, ok := w.mappedTarget("M2"); !ok {
		t.Fatal("M2 must NOT be closed by M1's drained closure")
	}
}

// When the master process is confirmed gone, teardown queues
// a closure for EVERY mapped tab (the last tab's destruction
// event may never have arrived) and the session ends with no
// mapping, worker or connection left behind.
func TestMasterGoneTeardownClosesAllMappedTabs(t *testing.T) {
	f := newFakeBrowserCDP(t)
	sess, w := sessionWithWorker(t, f)
	// Give the worker two mapped tabs, as a running session
	// would have (one pre-existing-style, one created).
	ctx := context.Background()
	for _, m := range []string{"MX-1", "MX-2"} {
		ft, err := w.ensureTarget(ctx, m, "https://example.test/")
		if err != nil {
			t.Fatalf("ensure %s: %v", m, err)
		}
		w.pairWith(m, ft)
	}
	svc := &SyncService{} // no instance runtime: master not running = confirmed gone
	svc.teardownMasterGone(sess, "主实例已退出（测试）")

	if n := f.closes.Load(); n != 2 {
		t.Fatalf("closes = %d, want 2 (every mapped tab closed)", n)
	}
	if got := w.mappedMasterTargets(); len(got) != 0 {
		t.Fatalf("mappings left after master-gone teardown: %v", got)
	}
	select {
	case <-w.loopDone:
	default:
		t.Fatal("worker dispatch loop must have ended")
	}
	if sess.followersSnapshot()["f1"] == nil {
		t.Fatal("worker record lost")
	}
	sess.mu.Lock()
	torn := sess.tornDown
	sess.mu.Unlock()
	if !torn {
		t.Fatal("session must be torn down")
	}
}
