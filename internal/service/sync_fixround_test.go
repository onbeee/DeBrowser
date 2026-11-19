package service

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// Tests for the PR #20 review-fix round. These are protocol
// simulation tests against the scripted fake Chromium endpoint —
// they do NOT execute the observer script and are NOT real
// browser tests. The observer script itself is covered by the
// Node unit tests in frontend/tests/syncCaptureScript.test.mjs,
// and real Chromium behaviour remains a Windows 11 manual
// acceptance item.

func TestPasteShortcutNeverDispatchedAndPasteTextSentOnce(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	_, _ = startTestSession(t, master, follower, "M1", "F1")

	// Even if a paste shortcut report slipped past the observer
	// script, the dispatch side must not send it: paste follows
	// the insertFromPaste text path only.
	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "v", Code: "KeyV", Ctrl: true})
	master.PushBindingEvent("M1", PageEvent{Kind: "keyup", Key: "v", Code: "KeyV", Ctrl: true})
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "粘贴内容", Source: "paste"})
	// Shift+Insert is the other paste shortcut: also withheld.
	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "Insert", Code: "Insert", Shift: true})

	text := follower.WaitCommands("Input.insertText", 1, 3*time.Second)
	if len(text) != 1 {
		t.Fatalf("insertText commands = %d, want exactly 1 (the paste increment)", len(text))
	}
	time.Sleep(200 * time.Millisecond)
	if got := follower.Commands("Input.dispatchKeyEvent"); len(got) != 0 {
		t.Fatalf("paste shortcuts produced %d key events, want 0 (single paste path)", len(got))
	}
	if got := follower.Commands("Input.insertText"); len(got) != 1 {
		t.Fatalf("paste produced %d inserts, want exactly 1", len(got))
	}
}

func TestCompositionTwinInputEventIsDeduped(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	_, _ = startTestSession(t, master, follower, "M1", "F1")

	// IME commit: compositionend reports the final text; the
	// engine's twin input event with the same text immediately
	// after must NOT insert it a second time.
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "你好", Source: "composition"})
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "你好", Source: "input"})
	if got := follower.WaitCommands("Input.insertText", 1, 3*time.Second); len(got) != 1 {
		t.Fatalf("composition produced %d inserts, want 1", len(got))
	}
	time.Sleep(200 * time.Millisecond)
	if got := follower.Commands("Input.insertText"); len(got) != 1 {
		t.Fatalf("composition twin was not deduped: %d inserts", len(got))
	}
	// A genuinely repeated commit later (a fresh keystroke in
	// between clears the twin marker) is still delivered.
	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "a", Code: "KeyA"})
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "你好", Source: "input"})
	if got := follower.WaitCommands("Input.insertText", 2, 3*time.Second); len(got) != 2 {
		t.Fatalf("genuine repeated input was eaten: %d inserts, want 2", len(got))
	}
}

func TestAttachIsSingleFlightUnderConcurrency(t *testing.T) {
	browser := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	browser.setAttachDelay(300 * time.Millisecond)
	ctx := context.Background()
	_, tracker, err := dialBrowser(ctx, browser.Port(), pageCallbacks{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// A targetCreated event and several direct attach calls race
	// for the same new target; exactly one attach may run.
	browser.PushTargetCreated("F9", "https://z.example/")
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs[i] = tracker.attach(c, "F9")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent attach %d: %v", i, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && browser.attachCount("F9") == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := browser.attachCount("F9"); got != 1 {
		t.Fatalf("attach commands for F9 = %d, want exactly 1 (single-flight)", got)
	}
	if _, ok := tracker.SessionFor("F9"); !ok {
		t.Fatal("attached target has no session")
	}
	tracker.mu.Lock()
	attaching := len(tracker.attaching)
	tracker.mu.Unlock()
	if attaching != 0 {
		t.Fatalf("attach bookkeeping leaked: %d calls still in flight", attaching)
	}
}

func TestAttachFailureLeavesNoResidueAndFiresNoCallback(t *testing.T) {
	browser := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	browser.failAttachFor("BAD")
	ctx := context.Background()
	wsURL, err := cdp.Discover(ctx, browser.Port())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	mux := &trackerMux{}
	client, err := cdp.Dial(ctx, wsURL, mux.handle)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()
	created := make(chan string, 4)
	tracker := newPageTracker(client, pageCallbacks{
		OnTargetCreated: func(id string) { created <- id },
	})
	mux.tr.Store(tracker)
	if err := client.SetDiscoverTargets(ctx, true); err != nil {
		t.Fatalf("discover targets: %v", err)
	}

	// Direct attach reports the failure.
	if err := tracker.attach(ctx, "BAD"); err == nil {
		t.Fatal("attach to a failing target must return an error")
	}
	// The event-driven path must not report a creation either.
	browser.PushTargetCreated("BAD", "https://bad.example/")
	select {
	case id := <-created:
		t.Fatalf("OnTargetCreated fired for failed attach of %q", id)
	case <-time.After(600 * time.Millisecond):
	}
	if _, ok := tracker.Pages()["BAD"]; ok {
		t.Fatal("failed attach left a tracked page behind")
	}
	if _, ok := tracker.SessionFor("BAD"); ok {
		t.Fatal("failed attach left a session mapping behind")
	}
	tracker.mu.Lock()
	residue := len(tracker.attaching) + len(tracker.contexts)
	tracker.mu.Unlock()
	if residue != 0 {
		t.Fatalf("failed attach left %d bookkeeping entries behind", residue)
	}
}

func TestCreatedTargetMappingWaitsForAttachReadiness(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	_, worker := startTestSession(t, master, follower, "M1", "F1")

	// The follower's attach now takes ~400ms: Target.createTarget
	// answers immediately, but the mapping must only appear once
	// the new page's session and tracking are fully attached.
	follower.setAttachDelay(400 * time.Millisecond)
	start := time.Now()
	master.PushTargetCreated("M9", "https://c.example/")
	var mapped string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ft, ok := worker.mappedTarget("M9"); ok {
			mapped = ft
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mapped == "" {
		t.Fatal("created master tab was never mapped")
	}
	if elapsed := time.Since(start); elapsed < 350*time.Millisecond {
		t.Fatalf("mapping appeared after %v, before the follower page was attached and ready", elapsed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.tracker.WaitReady(ctx, mapped); err != nil {
		t.Fatalf("mapped target is not ready: %v", err)
	}
	if _, ok := worker.tracker.SessionFor(mapped); !ok {
		t.Fatal("mapped target has no session")
	}
}

func TestPauseStopsAllStructuralSyncAndResumeSendsOnlyNewEvents(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")
	svc := sess.service
	svc.mu.Lock()
	svc.session = sess
	svc.mu.Unlock()

	// A second mapped tab exists before the pause.
	master.PushTargetCreated("M8", "https://d.example/")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := worker.mappedTarget("M8"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := worker.mappedTarget("M8"); !ok {
		t.Fatal("setup: second tab was not mapped before pause")
	}

	counts := func() (creates, closes, navs, activates int) {
		return len(follower.Commands("Target.createTarget")),
			len(follower.Commands("Target.closeTarget")),
			len(follower.Commands("Page.navigate")),
			len(follower.Commands("Target.activateTarget"))
	}
	c0, cl0, n0, a0 := counts()

	if err := svc.PauseSync(""); err != nil {
		t.Fatalf("PauseSync: %v", err)
	}
	// During the pause the master navigates, creates a tab,
	// closes a tab and switches tabs: NONE of it may execute on
	// the follower.
	master.PushFrameNavigated("M1", "https://a.example/paused", "loader-p")
	master.PushTargetCreated("M9", "https://e.example/")
	master.PushTargetDestroyed("M8")
	master.PushBindingEvent("M1", PageEvent{Kind: "visibility", State: "hidden", Href: "https://a.example/"})
	master.PushBindingEvent("M2", PageEvent{Kind: "visibility", State: "visible", Href: "https://b.example/"})
	time.Sleep(500 * time.Millisecond)
	c1, cl1, n1, a1 := counts()
	if c1 != c0 || cl1 != cl0 || n1 != n0 || a1 != a0 {
		t.Fatalf("paused session dispatched structural actions: creates %d->%d closes %d->%d navs %d->%d activates %d->%d",
			c0, c1, cl0, cl1, n0, n1, a0, a1)
	}

	// Resume re-confirms the master's current page (M2), then
	// only events produced AFTER the resume are dispatched: a
	// fresh navigation on the now-active master page goes
	// through, and the paused-period navigation is not replayed.
	if err := svc.ResumeSync(""); err != nil {
		t.Fatalf("ResumeSync: %v", err)
	}
	master.PushFrameNavigated("M2", "https://b.example/after", "loader-a")
	navs := follower.WaitCommands("Page.navigate", n0+1, 3*time.Second)
	if len(navs) != n0+1 {
		t.Fatalf("navigations after resume = %d, want %d (only the post-resume one)", len(navs), n0+1)
	}
	var np map[string]any
	_ = json.Unmarshal(navs[len(navs)-1].Params, &np)
	if np["url"] != "https://b.example/after" {
		t.Fatalf("post-resume navigation url = %v, want the new event only", np["url"])
	}
	if err := svc.StopSync(""); err != nil {
		t.Fatalf("StopSync: %v", err)
	}
}

func TestFollowerUnconfirmedRecoversWhenPageBecomesVisible(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	// The follower starts with NO visible page: its active page
	// cannot be confirmed at session start.
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "")
	sess, worker := startTestSessionWithUnconfirmedFollower(t, master, follower)
	_ = sess
	if state, _, _ := worker.snapshot(); state != followerUnconfirmed {
		t.Fatalf("follower state = %q, want unconfirmed at start", state)
	}
	// No input may flow while unconfirmed.
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "不应发送", Source: "input"})
	time.Sleep(250 * time.Millisecond)
	if got := follower.Commands("Input.insertText"); len(got) != 0 {
		t.Fatalf("unconfirmed follower received %d inserts", len(got))
	}
	// The follower's page becomes the unique visible page: the
	// follower re-confirms and becomes ready, paired with the
	// master's active page.
	follower.PushBindingEvent("F1", PageEvent{Kind: "visibility", State: "visible", Href: "https://a.example/"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if state, _, _ := worker.snapshot(); state == followerReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state, reason, _ := worker.snapshot(); state != followerReady {
		t.Fatalf("follower did not recover: state=%q reason=%q", state, reason)
	}
	if ft, ok := worker.mappedTarget("M1"); !ok || ft != "F1" {
		t.Fatalf("recovered mapping = %q ok=%v, want F1 paired with M1", ft, ok)
	}
	// And input flows again after recovery.
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "恢复后", Source: "input"})
	if got := follower.WaitCommands("Input.insertText", 1, 3*time.Second); len(got) != 1 {
		t.Fatalf("recovered follower received %d inserts, want 1", len(got))
	}
}

// startTestSessionWithUnconfirmedFollower wires a session whose
// follower has no visible page at start, mirroring startSync's
// unconfirmed path (worker created and registered before dial,
// so its visibility callbacks can drive recovery).
func startTestSessionWithUnconfirmedFollower(t *testing.T, master, follower *fakeChromium) (*syncSession, *followerWorker) {
	t.Helper()
	ctx := context.Background()
	svc := &SyncService{}
	sess := &syncSession{
		service:   svc,
		phase:     syncPhaseRunning,
		masterID:  "master",
		followers: map[string]*followerWorker{},
		order:     []string{"follower"},
	}
	mc, mt, err := dialBrowser(ctx, master.Port(), pageCallbacks{
		OnPageEvent:       sess.onMasterPageEvent,
		OnVisibility:      sess.onMasterVisibility,
		OnNavigate:        sess.onMasterNavigate,
		OnTargetCreated:   sess.onMasterTargetCreated,
		OnTargetDestroyed: sess.onMasterTargetDestroyed,
	})
	if err != nil {
		t.Fatalf("dial master: %v", err)
	}
	sess.masterClient = mc
	sess.setTracker(mt)
	active, _, ok := waitActiveTarget(mt, 3*time.Second)
	if !ok || active != "M1" {
		t.Fatalf("master active = %q ok=%v", active, ok)
	}
	sess.mu.Lock()
	sess.masterActive, sess.activeKnown = active, true
	sess.mu.Unlock()

	worker := newFollowerWorker("follower")
	sess.addFollower("follower", worker)
	fc, ft, err := dialBrowser(ctx, follower.Port(), sess.followerCallbacks("follower"))
	if err != nil {
		t.Fatalf("dial follower: %v", err)
	}
	worker.client, worker.tracker = fc, ft
	if _, _, found := waitActiveTarget(ft, 300*time.Millisecond); found {
		t.Fatal("setup: follower unexpectedly has a visible page")
	}
	worker.setUnconfirmed("无法确认跟随实例的活动页面")
	go worker.run()
	sess.live.Store(true)
	t.Cleanup(sess.teardown)
	return sess, worker
}

func TestSubframeExecutionContextIsNeverBound(t *testing.T) {
	browser := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	ctx := context.Background()
	_, _, err := dialBrowser(ctx, browser.Port(), pageCallbacks{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	bindingsFor := func(contextID int) int {
		n := 0
		for _, c := range browser.Commands("Runtime.addBinding") {
			var p map[string]any
			_ = json.Unmarshal(c.Params, &p)
			if id, ok := p["executionContextId"].(float64); ok && int(id) == contextID {
				n++
			}
		}
		return n
	}
	// A subframe's isolated-world context must not receive the
	// reporting binding (its frame ID is not the main frame's).
	browser.PushExecutionContextCreated("M1", "frame-CHILD", 77)
	time.Sleep(300 * time.Millisecond)
	if got := bindingsFor(77); got != 0 {
		t.Fatalf("subframe context received %d bindings, want 0", got)
	}
	// The main frame's own new context is bound as usual.
	browser.PushExecutionContextCreated("M1", "frame-M1", 78)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && bindingsFor(78) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := bindingsFor(78); got == 0 {
		t.Fatal("main-frame context was not bound")
	}
}
