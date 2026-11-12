// Package lifecycle provides the small concurrency primitives the
// application shell uses to own its background work: every goroutine
// is started with a context and reports its exit on a done channel,
// so shutdown is ordered and leak-free instead of fire-and-forget.
package lifecycle

import (
	"context"
	"time"
)

// RunTicker starts a goroutine that calls fn on every tick of
// interval until ctx is cancelled. The returned channel is closed
// when the goroutine has fully exited — after it closes, fn is
// guaranteed never to run again, so callers can safely release the
// resources fn used (database handles, files) once they observe it.
//
// fn runs on the loop goroutine; a slow fn simply delays later ticks
// (time.Ticker semantics). Cancelling ctx while fn is in flight does
// not interrupt fn; it runs to completion.
//
// The cancellation contract is observation-based, stated exactly:
// the loop starts fn only when cancellation has not been observed
// at the moment it decides. A cancellation that lands concurrently
// with that decision is indistinguishable from one that lands
// during fn itself — either way, that one callback may run to
// completion, and no callback starts after the loop has observed
// the cancellation. No stronger physical-time atomicity is claimed
// or needed: the load-bearing guarantee for callers is the done
// channel — once it closes, fn has finished and will never run
// again, so resources fn touched may be released after observing
// it (see WaitForever).
func RunTicker(ctx context.Context, interval time.Duration, fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// A tick and the cancellation can both be ready
				// when the select wakes; Go then chooses randomly.
				// Re-check the context: if cancellation has been
				// OBSERVED by this point, start no callback. A
				// cancellation racing this very check is treated
				// as landing during the callback (which is then
				// allowed to finish) — callers must rely on the
				// done channel, never on this check alone, before
				// releasing the resources fn uses.
				if ctx.Err() != nil {
					return
				}
				fn()
			}
		}
	}()
	return done
}

// WaitAll blocks until every done channel closes or ctx is done,
// whichever comes first. It returns true when all loops exited.
func WaitAll(ctx context.Context, dones ...<-chan struct{}) bool {
	for _, d := range dones {
		select {
		case <-d:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// WaitForever blocks until every done channel has closed. There is
// deliberately NO timeout escape: callers use it when the resources
// the loops touch (database, files) may only be released after the
// loops have provably stopped — returning early would not "time
// out" the loops, it would orphan them against closing resources.
// warn (may be nil) is invoked every warnEvery for as long as any
// loop is still running, purely as an observability signal; it
// never shortens the wait. A loop callback that never returns
// blocks the wait forever by design — loop callbacks must be
// bounded operations.
func WaitForever(warnEvery time.Duration, warn func(), dones ...<-chan struct{}) {
	for _, d := range dones {
		waitOne(d, warnEvery, warn)
	}
}

func waitOne(done <-chan struct{}, warnEvery time.Duration, warn func()) {
	if warnEvery <= 0 {
		<-done
		return
	}
	ticker := time.NewTicker(warnEvery)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if warn != nil {
				warn()
			}
		}
	}
}
