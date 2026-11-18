package service

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Master recovery ordering: when attaching the master's
// verified current target FAILS, the dispatch gate must stay
// closed, the old target must not become current, and the
// failure must be recorded for the sync state.
func TestVerifyAndAttachMasterFailureKeepsGateClosed(t *testing.T) {
	f := newRecordingCDP(t)
	f.failOn = "attachToTarget"
	client, err := dialRecording(t, f)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sess := &syncSession{
		masterClient: client,
		sessions:     map[string]string{},
		targetURLs:   map[string]string{},
		targetTitles: map[string]string{},
		workers:      map[string]*followerWorker{},
	}
	if sess.verifyAndAttachMaster("t1") {
		t.Fatal("verifyAndAttachMaster must fail when the attach fails")
	}
	if sess.tabMapOK.Load() {
		t.Fatal("dispatch gate opened despite a failed master attach")
	}
	if ct := sess.currentTargetSnapshot(); ct != "" {
		t.Fatalf("current target = %q, want none (old target must not be used)", ct)
	}
	if e := sess.masterErrSnapshot(); !strings.Contains(e, "附加主实例当前标签失败") {
		t.Fatalf("master error = %q, want an explicit attach failure", e)
	}
	// A later successful attach opens the gate and clears the
	// error — recovery through the same path the poller uses.
	f.failOn = ""
	if !sess.verifyAndAttachMaster("t1") {
		t.Fatal("verifyAndAttachMaster must succeed once attach works")
	}
	if !sess.tabMapOK.Load() {
		t.Fatal("dispatch gate must open after a successful attach")
	}
	if e := sess.masterErrSnapshot(); e != "" {
		t.Fatalf("master error = %q, want cleared", e)
	}
}

// A follower whose initial binding is missing must be visibly
// unready — not failed, not silently consuming input.
func TestWorkerInitWithoutBindingIsUnreadyNotSilent(t *testing.T) {
	f := newRecordingCDP(t)
	client, err := dialRecording(t, f)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	w := newFollowerWorker("f1", client, nil)
	go w.Run()
	defer func() { w.Stop(); <-w.done }()
	w.queue.push(syncEvent{kind: evInit, bindings: map[string]string{}, currentTarget: "m1"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		ready, cause := w.isReady()
		if !ready && strings.Contains(cause, "映射") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker readiness = %v/%q, want unready with a mapping cause", ready, cause)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed, _ := w.isFailed(); failed {
		t.Fatal("a missing binding must not fail the follower outright")
	}
	// Input offered now is refused at the gate — and the state
	// above says why, so nothing is silently dropped.
	w.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	time.Sleep(150 * time.Millisecond)
	if n := f.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("key events = %d, want 0 for an unattached follower", n)
	}
}

// A follower whose bound target cannot be attached fails
// explicitly at init.
func TestWorkerInitAttachFailureFails(t *testing.T) {
	f := newRecordingCDP(t)
	f.failOn = "attachToTarget"
	client, err := dialRecording(t, f)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	w := newFollowerWorker("f1", client, nil)
	go w.Run()
	defer func() { w.Stop(); <-w.done }()
	w.queue.push(syncEvent{kind: evInit, bindings: map[string]string{"m1": "t1"}, currentTarget: "m1"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		if failed, reason := w.isFailed(); failed {
			if !strings.Contains(reason, "连接跟随实例标签失败") {
				t.Fatalf("failure reason = %q", reason)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("worker with a failing attach must fail explicitly")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One unready follower never blocks a ready one, and a rebind
// with a verified binding restores only that follower.
func TestFollowerReadinessIsolationAndRebindRecovery(t *testing.T) {
	fReady := newRecordingCDP(t)
	fStuck := newRecordingCDP(t)
	wReady := dialWorker(t, "ready", fReady)
	wStuckF, err := dialRecording(t, fStuck)
	if err != nil {
		t.Fatal(err)
	}
	defer wStuckF.Close()
	wStuck := newFollowerWorker("stuck", wStuckF, nil)
	go wStuck.Run()
	defer func() { wStuck.Stop(); <-wStuck.done }()
	wStuck.queue.push(syncEvent{kind: evInit, bindings: map[string]string{}, currentTarget: "m1"})

	sess := &syncSession{
		workers: map[string]*followerWorker{"ready": wReady, "stuck": wStuck},
		order:   []string{"ready", "stuck"},
	}
	sess.tabMapOK.Store(true)
	keyEv := syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"}
	sess.broadcast(keyEv)
	fReady.waitFor(t, "dispatchKeyEvent", 1)
	time.Sleep(150 * time.Millisecond)
	if n := fStuck.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("unready follower received %d key events, want 0", n)
	}

	// A rebind WITHOUT the current target's binding changes
	// nothing; one WITH it attaches and opens only that gate.
	wStuck.Rebind(map[string]string{"other": "t1"}, "m1")
	time.Sleep(150 * time.Millisecond)
	if ready, _ := wStuck.isReady(); ready {
		t.Fatal("rebind without the current binding must not open the gate")
	}
	wStuck.Rebind(map[string]string{"m1": "t1"}, "m1")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ready, _ := wStuck.isReady(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not become ready after a verified rebind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.broadcast(keyEv)
	fStuck.waitFor(t, "dispatchKeyEvent", 1)
}

// Suspension is race-tight: once suspendInput has run, no
// concurrent broadcast can land a tab-bound event in the queue.
func TestSuspendInputRaceTight(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f) // ready
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					w.Enqueue(syncEvent{kind: evMouseMove, x: 1, y: 1})
				}
			}
		}()
	}
	w.suspendInput()
	close(stop)
	wg.Wait()
	// After suspension, further enqueues are refused at the
	// gate, and nothing tab-bound may sit in the queue.
	w.Enqueue(syncEvent{kind: evMouseMove, x: 2, y: 2})
	for w.queue.len() > 0 {
		ev, _ := w.queue.pop()
		if tabBoundInput(ev.kind) {
			t.Fatalf("tab-bound event in queue after suspension: kind %v", ev.kind)
		}
	}
	if ready, _ := w.isReady(); ready {
		t.Fatal("worker must report unready after suspension")
	}
}

// Service level: with no verifiable tab mapping anywhere
// (headless environment), StartSync must NOT guess the
// master's active target (no attach to the first target), the
// gate stays closed, and the follower reports suspended —
// never normal sync.
func TestStartSyncUnverifiedMappingGuessesNothing(t *testing.T) {
	fx := newSyncStartFixture(t)
	fx.start(t)
	if n := fx.mCDP.count("attachToTarget"); n != 0 {
		t.Fatalf("master attach calls = %d, want 0 (no unverified target may be attached)", n)
	}
	st, _ := fx.svc.GetSyncState("")
	if st.TabMapOK {
		t.Fatal("TabMapOK must be false while the mapping is unverified")
	}
	if len(st.Followers) != 1 {
		t.Fatalf("followers = %d, want 1", len(st.Followers))
	}
	if st.Followers[0].Status != "suspended" {
		t.Fatalf("follower status = %q, want suspended (not normal, not silent)", st.Followers[0].Status)
	}
	if st.Followers[0].Reason == "" {
		t.Fatal("a suspended follower must carry a reason")
	}
}
