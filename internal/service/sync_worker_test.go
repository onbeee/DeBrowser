package service

import (
	"context"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startedWorker returns a worker wired to the fake browser
// with two paired targets (M1→FT-1, M2→FT-2).
func startedWorker(t *testing.T, f *fakeBrowserCDP) *followerWorker {
	t.Helper()
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(w.close)
	for _, m := range []string{"M1", "M2"} {
		ft, err := w.ensureTarget(ctx, m, "https://example.test/")
		if err != nil {
			t.Fatalf("ensure %s: %v", m, err)
		}
		w.pairWith(m, ft)
	}
	w.start(ctx)
	return w
}

func TestQueueTailCoalescing(t *testing.T) {
	q := newSyncQueue(8)
	q.push(PageEvent{Kind: "pointermove", Source: "M", X: 1, Y: 1})
	q.push(PageEvent{Kind: "pointermove", Source: "M", X: 2, Y: 2})
	q.push(PageEvent{Kind: "wheel", Source: "M", X: 5, Y: 5, DeltaY: 100})
	q.push(PageEvent{Kind: "wheel", Source: "M", X: 6, Y: 6, DeltaY: 50})
	q.push(PageEvent{Kind: "keydown", Source: "M", Code: "KeyA"})
	if q.len() != 3 {
		t.Fatalf("queue len = %d, want 3 after coalescing", q.len())
	}
	mv, _ := q.pop(0)
	if mv.X != 2 || mv.Y != 2 {
		t.Fatalf("move = %+v, want the LATEST position", mv)
	}
	wh, _ := q.pop(0)
	if wh.DeltaY != 150 || wh.X != 6 {
		t.Fatalf("wheel = %+v, want merged deltas at latest position", wh)
	}
	// Moves for a DIFFERENT source never coalesce together.
	q.push(PageEvent{Kind: "pointermove", Source: "A", X: 1})
	q.push(PageEvent{Kind: "pointermove", Source: "B", X: 2})
	if q.len() != 3 { // keydown + 2 moves
		t.Fatalf("cross-source moves must not coalesce: len = %d", q.len())
	}
}

func TestPerTargetInputRelease(t *testing.T) {
	f := newFakeBrowserCDP(t)
	w := startedWorker(t, f)

	w.enqueue(PageEvent{Kind: "keydown", Source: "M1", Key: "a", Code: "KeyA"})
	w.enqueue(PageEvent{Kind: "keydown", Source: "M2", Key: "b", Code: "KeyB"})
	w.enqueue(PageEvent{Kind: "pointerdown", Source: "M1", Button: 0, Buttons: 1})
	waitFor(t, "presses dispatched", func() bool {
		return f.findCalls("Input.dispatchKeyEvent", "S-FT-1", "keyDown", "KeyA") == 1 &&
			f.findCalls("Input.dispatchKeyEvent", "S-FT-2", "keyDown", "KeyB") == 1 &&
			f.findCalls("Input.dispatchMouseEvent", "S-FT-1", "mousePressed", "") == 1
	})

	w.requestPurgeAndRelease()
	waitFor(t, "releases on the correct targets", func() bool {
		return f.findCalls("Input.dispatchKeyEvent", "S-FT-1", "keyUp", "KeyA") == 1 &&
			f.findCalls("Input.dispatchKeyEvent", "S-FT-2", "keyUp", "KeyB") == 1 &&
			f.findCalls("Input.dispatchMouseEvent", "S-FT-1", "mouseReleased", "") == 1
	})
	// No cross-target contamination.
	if n := f.findCalls("Input.dispatchKeyEvent", "S-FT-1", "keyUp", "KeyB"); n != 0 {
		t.Fatalf("KeyB released on the wrong target %d times", n)
	}
	if n := f.findCalls("Input.dispatchKeyEvent", "S-FT-2", "keyUp", "KeyA"); n != 0 {
		t.Fatalf("KeyA released on the wrong target %d times", n)
	}
	// Local state is empty afterwards.
	w.mu.Lock()
	left := len(w.inputs)
	w.mu.Unlock()
	if left != 0 {
		t.Fatalf("input state records left = %d", left)
	}
}

func TestTargetCloseReleasesOnlyItsOwnInputs(t *testing.T) {
	f := newFakeBrowserCDP(t)
	w := startedWorker(t, f)

	w.enqueue(PageEvent{Kind: "keydown", Source: "M1", Key: "a", Code: "KeyA"})
	w.enqueue(PageEvent{Kind: "keydown", Source: "M2", Key: "b", Code: "KeyB"})
	waitFor(t, "presses dispatched", func() bool {
		return f.findCalls("Input.dispatchKeyEvent", "S-FT-1", "keyDown", "KeyA") == 1 &&
			f.findCalls("Input.dispatchKeyEvent", "S-FT-2", "keyDown", "KeyB") == 1
	})

	w.enqueue(PageEvent{Kind: "targetclosed", Source: "M1"})
	waitFor(t, "close releases M1 inputs only", func() bool {
		return f.findCalls("Input.dispatchKeyEvent", "S-FT-1", "keyUp", "KeyA") == 1 &&
			f.closes.Load() >= 1
	})
	if n := f.findCalls("Input.dispatchKeyEvent", "S-FT-2", "keyUp", "KeyB"); n != 0 {
		t.Fatal("closing M1 must not release M2's inputs")
	}
	// M2's state survives and is released on demand.
	w.requestRelease()
	waitFor(t, "M2 release on demand", func() bool {
		return f.findCalls("Input.dispatchKeyEvent", "S-FT-2", "keyUp", "KeyB") == 1
	})
}

func TestCreateTargetCleanupOnAttachFailure(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.failEnable = true
	client := dialFake(t, f)
	w := newFollowerWorker("f1", client, nil)
	if _, err := w.ensureTarget(context.Background(), "M1", "https://example.test/"); err == nil {
		t.Fatal("attach failure must fail the creation")
	}
	if f.closes.Load() != 1 {
		t.Fatalf("half-created target closes = %d, want 1", f.closes.Load())
	}
	if _, ok := w.mappedTarget("M1"); ok {
		t.Fatal("failed creation must not leave a mapping")
	}
}

func TestTrackerAttachFailureDetachesSession(t *testing.T) {
	f := newFakeBrowserCDP(t)
	f.failEnable = true
	client := dialFake(t, f)
	tracker := newPageTracker(client, false, nil, nil)
	if err := tracker.Attach(context.Background(), "T9"); err == nil {
		t.Fatal("attach should fail when Page.enable fails")
	}
	if f.detaches.Load() != 1 {
		t.Fatalf("detaches = %d, want 1 (the established session must be cleaned up)", f.detaches.Load())
	}
	if _, ok := tracker.Session("T9"); ok {
		t.Fatal("failed attach must not retain the session")
	}
}
