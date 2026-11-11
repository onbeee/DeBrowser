package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunTickerTicksAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ticks atomic.Int64
	done := RunTicker(ctx, 10*time.Millisecond, func() { ticks.Add(1) })

	deadline := time.Now().Add(2 * time.Second)
	for ticks.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d ticks after 2s, want at least 3", ticks.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit within 2s of cancellation")
	}
	// After done closes, fn must never run again.
	stable := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if got := ticks.Load(); got != stable {
		t.Errorf("ticks continued after shutdown: %d -> %d", stable, got)
	}
}

func TestRunTickerCancelBeforeFirstTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var ticks atomic.Int64
	done := RunTicker(ctx, time.Hour, func() { ticks.Add(1) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop with pre-cancelled context did not exit")
	}
	if ticks.Load() != 0 {
		t.Errorf("ticks = %d, want 0", ticks.Load())
	}
}

func TestWaitForeverBlocksWhileCallbackInFlight(t *testing.T) {
	// The core lifecycle guarantee behind App.Run: cancelling the
	// context does NOT interrupt a callback that is already running,
	// and the drain must not report completion until it finishes —
	// otherwise the caller would close resources (the database)
	// under a live reader.
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var calls atomic.Int64
	done := RunTicker(ctx, 5*time.Millisecond, func() {
		calls.Add(1)
		entered <- struct{}{}
		<-release // simulate a slow in-flight status read
	})
	<-entered
	cancel()

	drained := make(chan struct{})
	go func() {
		WaitForever(20*time.Millisecond, nil, done)
		close(drained)
	}()
	// While the callback is blocked, the drain must NOT complete,
	// even across several warn intervals.
	select {
	case <-drained:
		t.Fatal("WaitForever returned while a callback was still in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForever did not return after the callback finished")
	}
	// And after the drain, the callback never runs again.
	stable := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != stable {
		t.Errorf("callback ran after drain: %d -> %d", stable, got)
	}
}

func TestWaitForeverMultipleLoopsAndWarn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var warns atomic.Int64
	d1 := RunTicker(ctx, time.Hour, func() {})
	d2 := RunTicker(ctx, time.Hour, func() {})
	d3 := RunTicker(ctx, time.Hour, func() {})
	time.AfterFunc(120*time.Millisecond, cancel)
	start := time.Now()
	WaitForever(30*time.Millisecond, func() { warns.Add(1) }, d1, d2, d3)
	if elapsed := time.Since(start); elapsed < 120*time.Millisecond {
		t.Errorf("WaitForever returned after %v, before cancellation took effect", elapsed)
	}
	if warns.Load() == 0 {
		t.Error("warn callback never fired during a 120ms drain with a 30ms interval")
	}
}

func TestWaitAllTimeoutDoesNotStopLoop(t *testing.T) {
	// A timed-out WaitAll reports false; the loop itself keeps its
	// own lifecycle and a later unbounded wait still completes —
	// timing out must never be mistaken for the loop having stopped.
	ctx, cancel := context.WithCancel(context.Background())
	done := RunTicker(ctx, time.Hour, func() {})
	short, shortCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer shortCancel()
	if WaitAll(short, done) {
		t.Fatal("WaitAll claimed completion while the loop was running")
	}
	select {
	case <-done:
		t.Fatal("loop exited merely because a WaitAll timed out")
	default:
	}
	cancel()
	WaitForever(10*time.Millisecond, nil, done)
}

func TestWaitAll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	noop := func() {}
	d1 := RunTicker(ctx, time.Hour, noop)
	d2 := RunTicker(ctx, time.Hour, noop)

	short, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	if WaitAll(short, d1, d2) {
		t.Error("WaitAll returned true while loops were still running")
	}
	cancel()
	long, longCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer longCancel()
	if !WaitAll(long, d1, d2) {
		t.Error("WaitAll returned false after all loops exited")
	}
}

// Regression test for the cancellation race in RunTicker: when a
// callback is in flight while cancellation lands AND a tick is
// pending, the loop must not start another callback afterwards.
// With the previous select-only implementation, both channels
// being ready meant Go could pick the tick branch (~50% per
// trial); the context re-check makes the outcome exact. Forty
// trials make the old behaviour's survival probability ~2^-40.
func TestRunTickerNoCallbackStartsAfterCancel(t *testing.T) {
	for trial := 0; trial < 40; trial++ {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var calls atomic.Int64
		done := RunTicker(ctx, time.Millisecond, func() {
			calls.Add(1)
			entered <- struct{}{}
			<-release // in-flight while cancellation lands
		})
		<-entered
		// Let several ticks elapse (the ticker buffers one) so a
		// tick is pending alongside the closed Done channel.
		time.Sleep(10 * time.Millisecond)
		cancel()
		close(release)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("trial %d: loop did not exit after cancellation", trial)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("trial %d: callback ran %d times, want exactly 1 (no new callback may start after cancellation)", trial, got)
		}
	}
}

// Several loops sharing one context all drain after cancellation,
// and WaitForever only returns once every one of them has exited.
func TestWaitForeverDrainsMultipleLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ticks atomic.Int64
	dones := make([]<-chan struct{}, 0, 4)
	for i := 0; i < 4; i++ {
		dones = append(dones, RunTicker(ctx, 2*time.Millisecond, func() { ticks.Add(1) }))
	}
	deadline := time.Now().Add(2 * time.Second)
	for ticks.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("loops did not tick (got %d)", ticks.Load())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	drained := make(chan struct{})
	go func() {
		WaitForever(10*time.Millisecond, nil, dones...)
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForever did not return after all loops exited")
	}
	for _, d := range dones {
		select {
		case <-d:
		default:
			t.Fatal("a loop's done channel was not closed when WaitForever returned")
		}
	}
}
