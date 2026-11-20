package service

import (
	"context"
	"testing"
	"time"
)

// sessionWithWorker returns a bare session with one started
// worker wired to the fake follower browser.
func sessionWithWorker(t *testing.T, f *fakeBrowserCDP) (*syncSession, *followerWorker) {
	t.Helper()
	sess := bareSession()
	sess.ctx, sess.cancel = context.WithCancel(context.Background())
	t.Cleanup(sess.cancel)
	w := newFollowerWorker("f1", dialFake(t, f), nil)
	sess.followers["f1"] = w
	w.start(sess.ctx)
	t.Cleanup(w.close)
	return sess, w
}

// The real-machine about:blank bug: a new tab's targetCreated
// arrives with an EMPTY url, and only targetInfoChanged later
// reveals the real one. No follower tab may be created from
// the transient empty URL; exactly one tab, with the final
// URL, is created when the update lands — and repeated
// lifecycle events never duplicate it.
func TestTargetCreationWaitsForFinalURL(t *testing.T) {
	f := newFakeBrowserCDP(t)
	sess, _ := sessionWithWorker(t, f)

	sess.noteTargetURL("T-NEW", "")
	sess.offerTarget("T-NEW")
	time.Sleep(150 * time.Millisecond)
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("created %d tabs from a transient empty URL, want 0", n)
	}

	sess.noteTargetURL("T-NEW", "chrome://newtab/")
	sess.offerTarget("T-NEW")
	waitFor(t, "one follower tab with the final URL", func() bool {
		return f.creates.Load() == 1
	})
	if got := f.createdURL(0); got != "chrome://newtab/" {
		t.Fatalf("follower tab URL = %q, want the master's final URL", got)
	}

	// Duplicate created/infoChanged offers for the same
	// target must not create a second tab.
	sess.offerTarget("T-NEW")
	sess.offerTarget("T-NEW")
	time.Sleep(150 * time.Millisecond)
	if n := f.creates.Load(); n != 1 {
		t.Fatalf("creates = %d after duplicate offers, want exactly 1", n)
	}
}

// Rapid consecutive new tabs each get exactly one counterpart.
func TestConsecutiveTargetCreations(t *testing.T) {
	f := newFakeBrowserCDP(t)
	sess, _ := sessionWithWorker(t, f)
	for i, tid := range []string{"T-A", "T-B", "T-C"} {
		sess.noteTargetURL(tid, "")
		sess.offerTarget(tid) // transient: nothing yet
		sess.noteTargetURL(tid, "https://example.test/"+tid)
		sess.offerTarget(tid)
		_ = i
	}
	waitFor(t, "three follower tabs", func() bool { return f.creates.Load() == 3 })
	for i, tid := range []string{"T-A", "T-B", "T-C"} {
		if got, want := f.createdURL(i), "https://example.test/"+tid; got != want {
			t.Fatalf("tab %d URL = %q, want %q", i, got, want)
		}
	}
}

// A target destroyed while still pending leaves nothing
// behind, and a destroyed target's URL record is gone.
func TestDestroyedPendingTargetCreatesNothing(t *testing.T) {
	f := newFakeBrowserCDP(t)
	sess, _ := sessionWithWorker(t, f)
	sess.noteTargetURL("T-TMP", "")
	sess.forgetTargetURL("T-TMP")
	sess.offerTarget("T-TMP")
	time.Sleep(120 * time.Millisecond)
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("destroyed pending target created %d tabs", n)
	}
	if got := sess.targetURL("T-TMP"); got != "" {
		t.Fatalf("destroyed target URL record = %q, want empty", got)
	}
}

// An empty URL reaching the worker is an error, never an
// about:blank tab.
func TestWorkerRefusesEmptyURLCreation(t *testing.T) {
	f := newFakeBrowserCDP(t)
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	if _, err := w.ensureTarget(context.Background(), "MX", ""); err == nil {
		t.Fatal("empty URL creation must fail")
	}
	if n := f.creates.Load(); n != 0 {
		t.Fatalf("creates = %d, want 0", n)
	}
}

// Right-button presses and the menu key are NOT dispatched to
// followers (they would open a native context menu that no
// page event can dismiss), while left-button input still is.
func TestRightButtonAndMenuKeyAreNotDispatched(t *testing.T) {
	f := newFakeBrowserCDP(t)
	w := startedWorker(t, f)

	w.enqueue(PageEvent{Kind: "pointerdown", Source: "M1", Button: 2, Buttons: 2})
	w.enqueue(PageEvent{Kind: "pointerup", Source: "M1", Button: 2, Buttons: 0})
	w.enqueue(PageEvent{Kind: "keydown", Source: "M1", Key: "ContextMenu", Code: "ContextMenu"})
	w.enqueue(PageEvent{Kind: "pointerdown", Source: "M1", Button: 0, Buttons: 1})
	w.enqueue(PageEvent{Kind: "pointerup", Source: "M1", Button: 0, Buttons: 0})

	waitFor(t, "left button dispatched", func() bool {
		return f.findCalls("Input.dispatchMouseEvent", "S-FT-1", "mousePressed", "") == 1 &&
			f.findCalls("Input.dispatchMouseEvent", "S-FT-1", "mouseReleased", "") == 1
	})
	// Exactly one press/release pair (the left button): the
	// right button produced nothing.
	f.callsMu.Lock()
	mouseCalls := 0
	keyCalls := 0
	for _, c := range f.calls {
		if c.Method == "Input.dispatchMouseEvent" {
			mouseCalls++
		}
		if c.Method == "Input.dispatchKeyEvent" {
			keyCalls++
		}
	}
	f.callsMu.Unlock()
	if mouseCalls != 2 {
		t.Fatalf("mouse input calls = %d, want exactly 2 (left press+release only)", mouseCalls)
	}
	if keyCalls != 0 {
		t.Fatalf("menu key produced %d key calls, want 0", keyCalls)
	}
	// The right button also left no pressed-state behind.
	w.mu.Lock()
	st := w.inputs["FT-1"]
	w.mu.Unlock()
	if st != nil && st.buttons != 0 {
		t.Fatalf("right button leaked pressed state: %+v", st)
	}
}
