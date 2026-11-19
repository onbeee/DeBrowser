package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// startTestSession wires a session between two fake browsers the
// same way startSync does, minus the instance bookkeeping.
func startTestSession(t *testing.T, master, follower *fakeChromium, masterActive, followerActive string) (*syncSession, *followerWorker) {
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
	if !ok || active != masterActive {
		t.Fatalf("master active = %q ok=%v, want %q", active, ok, masterActive)
	}
	sess.mu.Lock()
	sess.masterActive, sess.activeKnown = active, true
	if w, h, verr := mt.Viewport(ctx, active); verr == nil {
		sess.masterW, sess.masterH = w, h
	}
	sess.mu.Unlock()

	worker := newFollowerWorker("follower")
	sess.mu.Lock()
	worker.masterW, worker.masterH = sess.masterW, sess.masterH
	sess.mu.Unlock()
	sess.addFollower("follower", worker)
	fc, ft, err := dialBrowser(ctx, follower.Port(), sess.followerCallbacks("follower"))
	if err != nil {
		t.Fatalf("dial follower: %v", err)
	}
	worker.client, worker.tracker = fc, ft
	fActive, _, ok := waitActiveTarget(ft, 3*time.Second)
	if !ok || fActive != followerActive {
		t.Fatalf("follower active = %q ok=%v, want %q", fActive, ok, followerActive)
	}
	if w, h, verr := ft.Viewport(ctx, fActive); verr == nil {
		worker.viewportW, worker.viewportH = w, h
	}
	worker.mapTarget(masterActive, fActive)
	worker.setActiveTarget(fActive)
	worker.setReady()
	go worker.run()
	sess.live.Store(true)
	t.Cleanup(sess.teardown)
	return sess, worker
}

func mouseParams(t *testing.T, c fakeCommand) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(c.Params, &p); err != nil {
		t.Fatalf("params: %v", err)
	}
	return p
}

func TestSyncInputFlowScalesCoordinatesAndSendsTextOnce(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	master.setViewport("M1", 1600, 1200)
	follower.setViewport("F1", 800, 600)
	_, _ = startTestSession(t, master, follower, "M1", "F1")

	// Click at master (400,300) -> follower (200,150) at half scale.
	master.PushBindingEvent("M1", PageEvent{Kind: "pointerdown", X: 400, Y: 300, Button: 0, Buttons: 1, Detail: 1})
	master.PushBindingEvent("M1", PageEvent{Kind: "pointerup", X: 400, Y: 300, Button: 0, Buttons: 0, Detail: 1})
	// Committed text goes through insertText exactly once.
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "你好", Source: "input"})
	// Shortcuts AND bare printable keydowns are all key events
	// (games and canvas apps listen for them). The key event
	// carries no text payload, so it inserts nothing by itself;
	// the character still arrives only via the text event.
	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "a", Code: "KeyA", Ctrl: true})
	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "x", Code: "KeyX"})
	master.PushBindingEvent("M1", PageEvent{Kind: "keyup", Key: "x", Code: "KeyX"})
	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "Backspace", Code: "Backspace"})

	mouse := follower.WaitCommands("Input.dispatchMouseEvent", 2, 3*time.Second)
	if len(mouse) < 2 {
		t.Fatalf("follower mouse commands = %d, want >= 2", len(mouse))
	}
	down := mouseParams(t, mouse[0])
	if down["type"] != "mousePressed" || down["x"] != 200.0 || down["y"] != 150.0 {
		t.Fatalf("scaled press = %v", down)
	}
	text := follower.WaitCommands("Input.insertText", 1, 3*time.Second)
	if len(text) != 1 {
		t.Fatalf("insertText commands = %d, want 1", len(text))
	}
	var tp map[string]any
	_ = json.Unmarshal(text[0].Params, &tp)
	if tp["text"] != "你好" {
		t.Fatalf("insertText = %v", tp)
	}
	keys := follower.WaitCommands("Input.dispatchKeyEvent", 4, 3*time.Second)
	if len(keys) != 4 {
		t.Fatalf("key commands = %d, want 4 (shortcut, bare x down+up, Backspace)", len(keys))
	}
	// Key events must never carry a text payload: that is what
	// keeps a dispatched keydown from ALSO inserting its
	// character (the text event above is the single insert).
	for _, k := range keys {
		var kp map[string]any
		_ = json.Unmarshal(k.Params, &kp)
		if _, hasText := kp["text"]; hasText {
			t.Fatalf("key event carries a text payload (would duplicate input): %v", kp)
		}
		if _, hasText := kp["unmodifiedText"]; hasText {
			t.Fatalf("key event carries unmodifiedText (would duplicate input): %v", kp)
		}
	}
	// down/up stay paired for the bare character key.
	var xDown, xUp map[string]any
	_ = json.Unmarshal(keys[1].Params, &xDown)
	_ = json.Unmarshal(keys[2].Params, &xUp)
	if xDown["type"] != "keyDown" || xUp["type"] != "keyUp" || xDown["code"] != "KeyX" || xUp["code"] != "KeyX" {
		t.Fatalf("bare key pair = %v / %v, want keyDown+keyUp for KeyX", xDown, xUp)
	}
}

func TestMasterNavigationAndReloadSyncToFollower(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	_, _ = startTestSession(t, master, follower, "M1", "F1")

	master.PushFrameNavigated("M1", "https://a.example/next", "loader-1")
	navs := follower.WaitCommands("Page.navigate", 1, 3*time.Second)
	if len(navs) < 1 {
		t.Fatal("master navigation did not reach the follower")
	}
	var np map[string]any
	_ = json.Unmarshal(navs[0].Params, &np)
	if np["url"] != "https://a.example/next" {
		t.Fatalf("follower navigated to %v", np["url"])
	}
	// Same URL with a fresh loader is a reload, not a navigation.
	master.PushFrameNavigated("M1", "https://a.example/next", "loader-2")
	if got := follower.WaitCommands("Page.reload", 1, 3*time.Second); len(got) < 1 {
		t.Fatal("master reload did not reach the follower as Page.reload")
	}
	if got := follower.Commands("Page.navigate"); len(got) != 1 {
		t.Fatalf("reload produced %d navigations, want still 1", len(got))
	}
}

func TestMasterSwitchToUnmappedTabCreatesFollowerPage(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	_, worker := startTestSession(t, master, follower, "M1", "F1")

	// User switches to the pre-existing second tab on the master.
	master.PushBindingEvent("M1", PageEvent{Kind: "visibility", State: "hidden", Href: "https://a.example/"})
	master.PushBindingEvent("M2", PageEvent{Kind: "visibility", State: "visible", Href: "https://b.example/"})

	creates := follower.WaitCommands("Target.createTarget", 1, 3*time.Second)
	if len(creates) < 1 {
		t.Fatal("follower did not create a page for the unmapped master tab")
	}
	var cp map[string]any
	_ = json.Unmarshal(creates[0].Params, &cp)
	if cp["url"] != "https://b.example/" {
		t.Fatalf("created page url = %v, want the master tab's current URL", cp["url"])
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := worker.mappedTarget("M2"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ft, ok := worker.mappedTarget("M2"); !ok || ft == "" {
		t.Fatal("mapping for the switched tab was not established from the creation result")
	}
	if got := follower.WaitCommands("Target.activateTarget", 1, 3*time.Second); len(got) < 1 {
		t.Fatal("follower page was not activated after the switch")
	}
}

func TestMasterTabLifecycleCreatesAndClosesFollowerPages(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	_, worker := startTestSession(t, master, follower, "M1", "F1")

	master.PushTargetCreated("M9", "https://c.example/")
	deadline := time.Now().Add(3 * time.Second)
	var createdID string
	for time.Now().Before(deadline) {
		if ft, ok := worker.mappedTarget("M9"); ok {
			createdID = ft
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if createdID == "" {
		t.Fatal("new master tab was not mapped to a created follower page")
	}

	master.PushTargetDestroyed("M9")
	closes := follower.WaitCommands("Target.closeTarget", 1, 3*time.Second)
	if len(closes) < 1 {
		t.Fatal("closing the master tab did not close the follower page")
	}
	var cp map[string]any
	_ = json.Unmarshal(closes[0].Params, &cp)
	if cp["targetId"] != createdID {
		t.Fatalf("closed target = %v, want the mapped %q", cp["targetId"], createdID)
	}
}

func TestSameTitleAndShuffledTargetsDoNotAffectIdentity(t *testing.T) {
	// Both master pages share one title in the fake (every page is
	// titled 页面) and are returned in map order; identity still
	// resolves purely by target/session IDs.
	master := newFakeChromium(t, map[string]string{
		"M2": "https://b.example/", "M1": "https://a.example/",
	}, "M2")
	follower := newFakeChromium(t, map[string]string{"F1": "https://x.example/"}, "F1")
	sess, _ := startTestSession(t, master, follower, "M2", "F1")
	if sess.masterActive != "M2" {
		t.Fatalf("master active = %q, want M2", sess.masterActive)
	}
}

func TestAmbiguousVisibilityMeansNoActiveTarget(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	ctx := context.Background()
	_, tracker, err := dialBrowser(ctx, master.Port(), pageCallbacks{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, _, ok := waitActiveTarget(tracker, 3*time.Second); !ok {
		t.Fatal("expected a unique active target initially")
	}
	// A second top-level window becomes visible too: the active
	// page is now genuinely ambiguous and must not be guessed.
	master.PushBindingEvent("M2", PageEvent{Kind: "visibility", State: "visible", Href: "https://b.example/"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := tracker.ActiveTarget(); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("two visible pages must make the active target unconfirmed")
}

func TestWorkerCoalescesMovesAndPreservesCriticalOrder(t *testing.T) {
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	ctx := context.Background()
	client, tracker, err := dialBrowser(ctx, follower.Port(), pageCallbacks{})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	worker := newFollowerWorker("f")
	worker.client = client
	worker.tracker = tracker
	worker.setActiveTarget("F1")
	worker.setReady()
	// Enqueue before the loop starts: three moves collapse to one,
	// press and release keep their order behind it.
	worker.enqueueInput(PageEvent{Kind: "pointermove", X: 1, Y: 1}, "F1")
	worker.enqueueInput(PageEvent{Kind: "pointermove", X: 2, Y: 2}, "F1")
	worker.enqueueInput(PageEvent{Kind: "pointermove", X: 3, Y: 3}, "F1")
	worker.enqueueInput(PageEvent{Kind: "pointerdown", X: 3, Y: 3, Button: 0, Buttons: 1, Detail: 1}, "F1")
	worker.enqueueInput(PageEvent{Kind: "pointerup", X: 3, Y: 3, Button: 0, Buttons: 0, Detail: 1}, "F1")
	go worker.run()
	defer worker.stop()

	mouse := follower.WaitCommands("Input.dispatchMouseEvent", 3, 3*time.Second)
	if len(mouse) != 3 {
		t.Fatalf("mouse commands = %d, want exactly 3 (1 coalesced move + press + release)", len(mouse))
	}
	types := []string{}
	for _, c := range mouse {
		types = append(types, mouseParams(t, c)["type"].(string))
	}
	if strings.Join(types, ",") != "mouseMoved,mousePressed,mouseReleased" {
		t.Fatalf("order = %v", types)
	}
	if got := mouseParams(t, mouse[0])["x"]; got != 3.0 {
		t.Fatalf("coalesced move kept x = %v, want the latest (3)", got)
	}
}

func TestWorkerQueueOverloadFailsOnlyThatFollower(t *testing.T) {
	stuck := newFollowerWorker("stuck")
	stuck.setReady()
	for i := 0; i < followerQueueCapacity; i++ {
		stuck.enqueueInput(PageEvent{Kind: "keydown", Key: "F1", Code: "F1"}, "T")
	}
	stuck.enqueueInput(PageEvent{Kind: "keydown", Key: "F2", Code: "F2"}, "T")
	if state, _, _ := stuck.snapshot(); state != followerFailed {
		t.Fatalf("overloaded follower state = %q, want failed (critical events are never silently dropped)", state)
	}
	healthy := newFollowerWorker("healthy")
	healthy.setReady()
	healthy.enqueueInput(PageEvent{Kind: "keydown", Key: "F1", Code: "F1"}, "T")
	if state, _, _ := healthy.snapshot(); state != followerReady {
		t.Fatalf("healthy follower state = %q, want ready", state)
	}
}

func TestPauseReleasesPressedKeysAndDropsStaleInput(t *testing.T) {
	master := newFakeChromium(t, map[string]string{"M1": "https://a.example/"}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, _ := startTestSession(t, master, follower, "M1", "F1")
	svc := sess.service
	svc.mu.Lock()
	svc.session = sess
	svc.mu.Unlock()

	master.PushBindingEvent("M1", PageEvent{Kind: "keydown", Key: "Control", Code: "ControlLeft", Ctrl: true})
	if got := follower.WaitCommands("Input.dispatchKeyEvent", 1, 3*time.Second); len(got) < 1 {
		t.Fatal("keydown was not dispatched before pause")
	}
	if err := svc.PauseSync(""); err != nil {
		t.Fatalf("PauseSync: %v", err)
	}
	// The pause releases the held Control key on the follower.
	keys := follower.WaitCommands("Input.dispatchKeyEvent", 2, 3*time.Second)
	if len(keys) < 2 {
		t.Fatal("pause did not release the pressed key")
	}
	var rel map[string]any
	_ = json.Unmarshal(keys[1].Params, &rel)
	if rel["type"] != "keyUp" {
		t.Fatalf("release event = %v, want keyUp", rel)
	}
	// Input during the pause is dropped at intake, not replayed
	// on resume. Give the intake a moment to observe (and drop)
	// the event while the session is still paused.
	master.PushBindingEvent("M1", PageEvent{Kind: "text", Text: "不应重放"})
	time.Sleep(300 * time.Millisecond)
	if err := svc.ResumeSync(""); err != nil {
		t.Fatalf("ResumeSync: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := follower.Commands("Input.insertText"); len(got) != 0 {
		t.Fatalf("paused input was replayed: %d insertText commands", len(got))
	}
	if err := svc.StopSync(""); err != nil {
		t.Fatalf("StopSync: %v", err)
	}
}

func TestStartSyncValidation(t *testing.T) {
	instances, cfg := testInstanceService(t)
	browserID := testBrowserID(t, cfg)
	sync := NewSyncService(instances, nil)

	if err := sync.StartSync("", "", []string{"x"}); err == nil || !strings.Contains(err.Error(), "主实例") {
		t.Fatalf("empty master error = %v", err)
	}
	if err := sync.StartSync("", "m", nil); err == nil || !strings.Contains(err.Error(), "跟随实例") {
		t.Fatalf("empty followers error = %v", err)
	}
	if err := sync.StartSync("", "m", []string{"m"}); err == nil || !strings.Contains(err.Error(), "不能重复") {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := sync.StartSync("", "ghost-master", []string{"ghost-follower"}); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("missing instance error = %v", err)
	}
	// A stopped instance cannot join.
	master, err := instances.CreateInstance("", CreateInstanceInput{Name: "主", BrowserID: browserID})
	if err != nil {
		t.Fatalf("create master: %v", err)
	}
	followerInst, err := instances.CreateInstance("", CreateInstanceInput{Name: "从", BrowserID: browserID})
	if err != nil {
		t.Fatalf("create follower: %v", err)
	}
	if err := sync.StartSync("", master.ID, []string{followerInst.ID}); err == nil || !strings.Contains(err.Error(), "未在运行") {
		t.Fatalf("stopped instance error = %v", err)
	}
	// A session in progress blocks a second start.
	sync.session = &syncSession{}
	if err := sync.StartSync("", master.ID, []string{followerInst.ID}); err == nil || !strings.Contains(err.Error(), "已在进行中") {
		t.Fatalf("concurrent session error = %v", err)
	}
	sync.session = nil
}
