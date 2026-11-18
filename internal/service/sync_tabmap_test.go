package service

import (
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/capture"
)

// While the tab mapping is unverified, tab-bound input (page
// mouse/keyboard/navigation) must not be dispatched at all —
// and dispatch resumes when the mapping verifies again.
func TestBroadcastGatedByTabMap(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	sess := &syncSession{
		workers: map[string]*followerWorker{"f1": w},
		order:   []string{"f1"},
	}
	sess.tabMapOK.Store(true)

	sess.broadcast(syncEvent{kind: evMouseMove, x: 10, y: 10})
	f.waitFor(t, "mouseMoved", 1)

	sess.setTabMapOK(false)
	sess.broadcast(syncEvent{kind: evMouseMove, x: 20, y: 20})
	sess.broadcast(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	sess.broadcast(syncEvent{kind: evNavigate, url: "https://evil.example/"})
	time.Sleep(200 * time.Millisecond)
	if n := f.count("mouseMoved"); n != 1 {
		t.Fatalf("mouseMoved = %d, want 1 (dispatch must stop while unmapped)", n)
	}
	if n := f.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("key events = %d, want 0 while unmapped", n)
	}
	if n := f.count("Page.navigate"); n != 0 {
		t.Fatalf("navigations = %d, want 0 while unmapped", n)
	}

	// Still unmapped on a repeated loss: stays gated.
	sess.setTabMapOK(false)
	sess.broadcast(syncEvent{kind: evMouseMove, x: 30, y: 30})
	time.Sleep(100 * time.Millisecond)
	if n := f.count("mouseMoved"); n != 1 {
		t.Fatalf("mouseMoved = %d, want 1 (persistent failure stays gated)", n)
	}

	// Mapping verified again: dispatch resumes.
	sess.setTabMapOK(true)
	sess.broadcast(syncEvent{kind: evMouseMove, x: 40, y: 40})
	f.waitFor(t, "mouseMoved", 2)
}

// Losing the mapping drops already-queued tab-bound input from
// the follower and injects a release — a stale target is never
// driven with pending input.
func TestTabMapLossClearsQueuedInput(t *testing.T) {
	f := newRecordingCDP(t)
	client, err := dialRecording(t, f)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	w := newFollowerWorker("f1", client, nil) // not Run: queue is inspected directly
	w.queue.push(syncEvent{kind: evMouseMove, x: 1})
	w.queue.push(syncEvent{kind: evKey, vk: 65, down: true})
	sess := &syncSession{
		workers: map[string]*followerWorker{"f1": w},
		order:   []string{"f1"},
	}
	sess.tabMapOK.Store(true)
	sess.setTabMapOK(false)
	for w.queue.len() > 0 {
		ev, _ := w.queue.pop()
		if ev.kind == evMouseMove || ev.kind == evKey {
			t.Fatalf("queued input survived mapping loss: %+v", ev)
		}
	}
	// The only thing that may remain is the release control event.
}

// Pause must never drop a pending evInit: a worker paused
// before initialization still attaches afterwards, exactly once.
func TestClearInputKeepsInit(t *testing.T) {
	f := newRecordingCDP(t)
	client, err := dialRecording(t, f)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	w := newFollowerWorker("f1", client, nil)
	w.queue.push(syncEvent{kind: evInit, bindings: map[string]string{"m1": "t1"}, currentTarget: "m1", masterW: 100, masterH: 100})
	w.queue.push(syncEvent{kind: evMouseMove, x: 1})
	w.queue.clearInput() // pause
	w.queue.clearInput() // pause again
	go w.Run()
	defer func() { w.Stop(); <-w.done }()
	f.waitFor(t, "attachToTarget", 1)
	time.Sleep(100 * time.Millisecond)
	if n := f.count("attachToTarget"); n != 1 {
		t.Fatalf("attach calls = %d, want exactly 1 (no duplicate init)", n)
	}
}

// Session capture lifecycle: teardown before start refuses the
// start; stop is idempotent and pairs exactly once.
func TestSessionCaptureLifecycleGuard(t *testing.T) {
	svc := NewSyncService(nil)
	starts, stops := 0, 0
	svc.captureStart = func(h capture.Handler) error { starts++; return nil }
	svc.captureStop = func() { stops++ }

	ended := &syncSession{svc: svc}
	ended.stopCapture() // teardown ran first
	if err := ended.startCapture(); err != errCaptureEnded {
		t.Fatalf("startCapture after teardown = %v, want errCaptureEnded", err)
	}
	if starts != 0 {
		t.Fatalf("capture starts = %d, want 0 for a dead session", starts)
	}

	live := &syncSession{svc: svc}
	if err := live.startCapture(); err != nil {
		t.Fatal(err)
	}
	live.stopCapture()
	live.stopCapture() // idempotent
	if starts != 1 || stops != 1 {
		t.Fatalf("starts/stops = %d/%d, want 1/1", starts, stops)
	}
}

// The unsynced counter counts only operations that were
// attempted and failed — once each.
func TestUnsyncedCounterCountsOpFailuresOnly(t *testing.T) {
	fx := newSyncStartFixture(t)
	fx.start(t)
	st, _ := fx.svc.GetSyncState("")
	if st.UISkipped != 0 {
		t.Fatalf("UISkipped = %d at session start, want 0", st.UISkipped)
	}
	fx.svc.mu.Lock()
	sess := fx.svc.session
	fx.svc.mu.Unlock()
	fx.svc.noteOpFailed(sess, fx.follower.ID, "打开浏览器菜单", "打开浏览器菜单未执行：找不到控件")
	fx.svc.noteOpFailed(sess, fx.follower.ID, "切换标签", "该标签未建立同步映射，无法可靠切换")
	st, _ = fx.svc.GetSyncState("")
	if st.UISkipped != 2 {
		t.Fatalf("UISkipped = %d, want 2 (one per failed operation)", st.UISkipped)
	}
}
