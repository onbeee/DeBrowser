package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/winmgr"
)

func bareSession() *syncSession {
	return &syncSession{
		phase:        syncRunning,
		systemPauses: map[string]bool{},
		followers:    map[string]*followerWorker{},
		navSeen:      map[string]time.Time{},
	}
}

func TestCompositionTwinDedup(t *testing.T) {
	sess := bareSession()
	w := newFollowerWorker("f1", nil, nil)
	sess.followers["f1"] = w

	sess.dispatch(PageEvent{Kind: "compositionend", Source: "M", Text: "你好"})
	sess.dispatch(PageEvent{Kind: "input", Source: "M", Text: "你好"}) // twin: dropped
	sess.dispatch(PageEvent{Kind: "input", Source: "M", Text: "!"})  // real: kept

	got := []PageEvent{}
	for i := 0; i < 2; i++ {
		got = append(got, <-w.queue)
	}
	if got[0].Kind != "compositionend" || got[1].Text != "!" {
		t.Fatalf("queue = %+v, want compositionend + '!'", got)
	}
	select {
	case extra := <-w.queue:
		t.Fatalf("unexpected third event: %+v", extra)
	default:
	}
}

func TestManualPauseSurvivesSystemPauseCycle(t *testing.T) {
	sess := bareSession()
	if !sess.live() {
		t.Fatal("running session should be live")
	}
	sess.setSystemPause("dialog", true)
	if sess.live() {
		t.Fatal("dialog pause must stop liveness")
	}
	sess.setSystemPause("dialog", false)
	if !sess.live() {
		t.Fatal("dialog end should restore liveness when no manual pause")
	}
	sess.manualPaused = true
	sess.setSystemPause("dialog", true)
	sess.setSystemPause("dialog", false)
	if sess.live() {
		t.Fatal("manual pause must survive the system pause cycle")
	}
	if sess.phaseLabel() != "已暂停" {
		t.Fatalf("label = %q", sess.phaseLabel())
	}
}

func TestWorkerQueueOverflowFailsOnlyThatFollower(t *testing.T) {
	failed := make(chan string, 1)
	w := newFollowerWorker("f1", nil, func(id, reason string) { failed <- id })
	for i := 0; i < 300; i++ {
		w.enqueue(PageEvent{Kind: "keyup", Source: "M", Code: "KeyA"})
	}
	if !w.isFailed() {
		t.Fatal("critical overflow must fail the follower")
	}
	select {
	case id := <-failed:
		if id != "f1" {
			t.Fatalf("failed id = %q", id)
		}
	default:
		t.Fatal("onFail not called")
	}
	// Pointer moves are non-critical: dropping them must not
	// fail a fresh worker.
	w2 := newFollowerWorker("f2", nil, nil)
	for i := 0; i < 300; i++ {
		w2.enqueue(PageEvent{Kind: "pointermove", Source: "M", X: 1, Y: 2})
	}
	if w2.isFailed() {
		t.Fatal("pointer moves must never fail the follower")
	}
}

func TestEnsureTargetSingleFlightAndFailureCleanup(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.delay = 300 * time.Millisecond
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	ctx := context.Background()

	// Three concurrent triggers share ONE creation.
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tid, err := w.ensureTarget(ctx, "M1", "https://example.test/")
			if err != nil {
				t.Errorf("ensure: %v", err)
			}
			results[i] = tid
		}(i)
	}
	wg.Wait()
	if f.creates.Load() != 1 {
		t.Fatalf("createTarget calls = %d, want exactly 1", f.creates.Load())
	}
	if results[0] == "" || results[0] != results[1] || results[1] != results[2] {
		t.Fatalf("shared result mismatch: %v", results)
	}
	w.pairWith("M1", results[0])

	// A second master target whose creation fails leaves no
	// mapping and no in-flight residue, and can be retried.
	f.delay = 0
	f.fail = true
	if _, err := w.ensureTarget(ctx, "M2", "https://example.test/"); err == nil {
		t.Fatal("failing creation should error")
	}
	if _, ok := w.mappedTarget("M2"); ok {
		t.Fatal("failed creation must not leave a mapping")
	}
	w.mu.Lock()
	left := len(w.creating)
	w.mu.Unlock()
	if left != 0 {
		t.Fatalf("creating residue = %d", left)
	}
	f.fail = false
	tid, err := w.ensureTarget(ctx, "M2", "https://example.test/")
	if err != nil || tid == "" {
		t.Fatalf("retry after failure = %q, %v", tid, err)
	}
}

func TestTrackerAttachSingleFlight(t *testing.T) {
	f := newFakeBrowserCDP(t)
	client := dialFake(t, f)
	tracker := newPageTracker(client, false, nil, nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = tracker.Attach(ctx, "T1")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
	}
	if !tracker.IsReady("T1") {
		t.Fatal("target should be ready after attach")
	}
	if got := tracker.VisibleTarget(); got != "" {
		t.Fatalf("no visibility reported, VisibleTarget = %q, want \"\"", got)
	}
}

func TestRestoreWindowsRetainsOnlyFailedRecords(t *testing.T) {
	s := &SyncService{savedPlacements: map[uintptr]winmgr.Placement{}}
	pl := winmgr.Placement{ShowCmd: winmgr.ShowNormal, NormalRect: winmgr.Rect{X: 1, Y: 2, Width: 3, Height: 4}}
	s.savedPlacements[101] = pl
	s.savedPlacements[102] = pl
	s.savedPlacements[103] = pl
	attempts := map[uintptr]int{}
	s.setWindowPlacement = func(hwnd uintptr, p winmgr.Placement) error {
		attempts[hwnd]++
		if hwnd == 102 {
			return errTestRestore
		}
		return nil
	}
	if err := s.restore(); err == nil {
		t.Fatal("restore with one failure should report it")
	}
	if len(s.savedPlacements) != 1 {
		t.Fatalf("records left = %d, want only the failed one", len(s.savedPlacements))
	}
	if _, ok := s.savedPlacements[102]; !ok {
		t.Fatal("the failed window's record must be retained verbatim")
	}
	for _, hwnd := range []uintptr{101, 102, 103} {
		if attempts[hwnd] != 1 {
			t.Fatalf("window %d attempted %d times, want 1", hwnd, attempts[hwnd])
		}
	}
	// Retry succeeds and clears the record.
	s.setWindowPlacement = func(hwnd uintptr, p winmgr.Placement) error { return nil }
	if err := s.restore(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(s.savedPlacements) != 0 {
		t.Fatal("all records should be gone after a full restore")
	}
	if err := s.restore(); err == nil {
		t.Fatal("restore with no records should say so")
	}
}

var errTestRestore = &testError{"restore failed"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }
