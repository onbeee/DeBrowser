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
