package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// switchFixture builds a session whose master is attached to
// target "m1" with the gate open and one ready follower.
func switchFixture(t *testing.T) (*syncSession, *recordingCDP, *recordingCDP, *followerWorker) {
	t.Helper()
	mCDP := newRecordingCDP(t)
	mClient, err := dialRecording(t, mCDP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mClient.Close() })
	fCDP := newRecordingCDP(t)
	w := dialWorker(t, "f1", fCDP)
	_, isvc, _ := bookmarkTestServices(t)
	sess := &syncSession{
		svc:          NewSyncService(isvc),
		masterClient: mClient,
		sessions:     map[string]string{},
		targetURLs:   map[string]string{"m1": "https://example.com/"},
		targetTitles: map[string]string{"m1": "Example"},
		workers:      map[string]*followerWorker{"f1": w},
		order:        []string{"f1"},
		opErrors:     map[string]string{},
	}
	if err := sess.attachMasterTarget("m1"); err != nil {
		t.Fatal(err)
	}
	sess.tabMapOK.Store(true)
	return sess, mCDP, fCDP, w
}

var (
	switchTitles = []string{"Example"}
	switchInfos  = []cdp.TargetInfo{{TargetID: "m2", Type: "page", Title: "Example"}}
)

// During a master tab switch the follower's old readiness must
// be gone BEFORE the master finishes attaching the new target:
// input broadcast mid-switch reaches nothing.
func TestSwitchSuspendsFollowersBeforeMasterAttach(t *testing.T) {
	sess, mCDP, fCDP, w := switchFixture(t)
	mCDP.delay = 300 * time.Millisecond // master attach is slow

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.advanceToTarget(switchTitles, switchInfos, 0, "m2")
	}()
	// The switch is in flight (master attach pending). The
	// follower must already be suspended.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ready, _ := w.isReady(); !ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower stayed ready during the master tab switch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sess.broadcast(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	<-done
	time.Sleep(100 * time.Millisecond)
	if n := fCDP.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("key events during switch = %d, want 0 (old target must receive nothing)", n)
	}
	if ct := sess.currentTargetSnapshot(); ct != "m2" {
		t.Fatalf("master current target = %q, want m2", ct)
	}
	if !sess.tabMapOK.Load() {
		t.Fatal("session gate must be open after a successful master attach")
	}
	// The follower could not rebind from the headless data
	// (no follower tab strip), so it stays suspended — the
	// session gate alone never makes it ready.
	if ready, _ := w.isReady(); ready {
		t.Fatal("follower must stay unready until its own re-attachment")
	}
}

// When the master attach to the new target fails, every gate
// stays closed.
func TestSwitchMasterAttachFailureKeepsAllGatesClosed(t *testing.T) {
	sess, mCDP, fCDP, w := switchFixture(t)
	mCDP.failOn = "attachToTarget"
	sess.advanceToTarget(switchTitles, switchInfos, 0, "m2")
	if sess.tabMapOK.Load() {
		t.Fatal("session gate opened despite a failed master attach")
	}
	if ready, _ := w.isReady(); ready {
		t.Fatal("follower ready despite a failed master attach")
	}
	if e := sess.masterErrSnapshot(); !strings.Contains(e, "附加主实例当前标签失败") {
		t.Fatalf("master error = %q, want the attach failure reported", e)
	}
	sess.broadcast(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	time.Sleep(100 * time.Millisecond)
	if n := fCDP.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("key events = %d, want 0", n)
	}
}

// After the master has switched, a follower receives input
// only once IT has re-attached to the new target's counterpart;
// the input then lands after that attach, never before.
func TestSwitchFollowerRecoversOnlyAfterOwnReattach(t *testing.T) {
	sess, _, fCDP, w := switchFixture(t)
	sess.advanceToTarget(switchTitles, switchInfos, 0, "m2")
	// Follower not yet re-attached: input refused.
	sess.broadcast(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	time.Sleep(100 * time.Millisecond)
	if n := fCDP.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("key events before follower re-attach = %d, want 0", n)
	}
	// Complete this follower's recovery (verified binding for
	// the new master target m2 -> its own target t1).
	attachBefore := fCDP.count("attachToTarget")
	w.Rebind(map[string]string{"m2": "t1"}, "m2")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ready, _ := w.isReady(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("follower did not recover after its rebind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fCDP.count("attachToTarget") != attachBefore+1 {
		t.Fatal("recovery must re-attach the follower to the new target's counterpart")
	}
	sess.broadcast(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	fCDP.waitFor(t, "dispatchKeyEvent", 1)
}

// Destroying the master's CURRENT target closes the dispatch
// gate synchronously in the event handler — no poll cycle.
func TestCurrentTargetDestroyedClosesGateImmediately(t *testing.T) {
	sess, _, fCDP, w := switchFixture(t)
	params, _ := json.Marshal(map[string]string{"targetId": "m1"})
	sess.onMasterEvent(cdp.Event{Method: "Target.targetDestroyed", Params: params})
	if sess.tabMapOK.Load() {
		t.Fatal("gate still open after the current target was destroyed")
	}
	if ct := sess.currentTargetSnapshot(); ct != "" {
		t.Fatalf("current target = %q, want cleared", ct)
	}
	if ready, _ := w.isReady(); ready {
		t.Fatal("follower still ready after the current target was destroyed")
	}
	sess.broadcast(syncEvent{kind: evMouseMove, x: 5, y: 5})
	time.Sleep(100 * time.Millisecond)
	if n := fCDP.count("mouseMoved"); n != 0 {
		t.Fatalf("mouse events after target destruction = %d, want 0", n)
	}
	// Destroying a NON-current target does not close the gate.
	sess2, _, _, _ := switchFixture(t)
	params2, _ := json.Marshal(map[string]string{"targetId": "other"})
	sess2.onMasterEvent(cdp.Event{Method: "Target.targetDestroyed", Params: params2})
	if !sess2.tabMapOK.Load() {
		t.Fatal("gate closed by an unrelated target destruction")
	}
}

// One follower's failed recovery never blocks another's.
func TestSwitchRecoveryIsPerFollower(t *testing.T) {
	sess, _, fCDP, w := switchFixture(t)
	f2 := newRecordingCDP(t)
	w2 := dialWorker(t, "f2", f2)
	sess.workers["f2"] = w2
	sess.order = append(sess.order, "f2")

	sess.advanceToTarget(switchTitles, switchInfos, 0, "m2")
	// w recovers; w2's rebind has no binding for m2.
	w.Rebind(map[string]string{"m2": "t1"}, "m2")
	w2.Rebind(map[string]string{}, "m2")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ready, _ := w.isReady(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first follower did not recover")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if ready, cause := w2.isReady(); ready {
		t.Fatalf("second follower ready without a binding (cause %q)", cause)
	}
	sess.broadcast(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	fCDP.waitFor(t, "dispatchKeyEvent", 1)
	if n := f2.count("dispatchKeyEvent"); n != 0 {
		t.Fatalf("unrecovered follower received %d key events, want 0", n)
	}
}
