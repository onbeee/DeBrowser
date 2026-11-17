package service

import (
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/wininput"
)

// The hook callback path (queue.push) must return immediately
// even when the consumer is stalled and the queue is full.
func TestInputQueuePushNeverBlocks(t *testing.T) {
	q := newInputEventQueue(8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5000; i++ {
			kind := wininput.EventMouseMove
			if i%3 == 0 {
				kind = wininput.EventKeyDown
			}
			q.push(wininput.Event{Kind: kind, VK: 65})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("push blocked under congestion: the hook callback would stall")
	}
	if q.droppedCount() == 0 {
		t.Fatal("overflow must have evicted events")
	}
	if got := len(q.drain()); got > 8 {
		t.Fatalf("queue holds %d events, capacity is 8", got)
	}
}

// Release events are the last thing the queue may drop: losing a
// key-up or button-up would stick input on every follower.
func TestInputQueueProtectsReleases(t *testing.T) {
	q := newInputEventQueue(4)
	q.push(wininput.Event{Kind: wininput.EventKeyUp, VK: 65})
	q.push(wininput.Event{Kind: wininput.EventMouseMove})
	q.push(wininput.Event{Kind: wininput.EventMouseMove})
	q.push(wininput.Event{Kind: wininput.EventMouseDown, Button: "left"})
	// Overflow with moves and presses: the key-up must survive.
	for i := 0; i < 10; i++ {
		q.push(wininput.Event{Kind: wininput.EventMouseMove})
		q.push(wininput.Event{Kind: wininput.EventKeyDown, VK: 66})
	}
	found := false
	for _, ev := range q.drain() {
		if ev.Kind == wininput.EventKeyUp && ev.VK == 65 {
			found = true
		}
	}
	if !found {
		t.Fatal("the queued key-up was evicted while disposable events remained")
	}
}

func TestInputQueueOrderPreservedWhenNotFull(t *testing.T) {
	q := newInputEventQueue(16)
	want := []wininput.EventKind{
		wininput.EventKeyDown, wininput.EventMouseDown,
		wininput.EventMouseUp, wininput.EventKeyUp,
	}
	for _, k := range want {
		q.push(wininput.Event{Kind: k})
	}
	got := q.drain()
	if len(got) != len(want) {
		t.Fatalf("drained %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Kind != want[i] {
			t.Fatalf("event %d = %v, want %v", i, got[i].Kind, want[i])
		}
	}
	if q.droppedCount() != 0 {
		t.Fatal("nothing should be dropped below capacity")
	}
}
