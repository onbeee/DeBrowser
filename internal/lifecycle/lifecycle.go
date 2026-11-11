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
// not interrupt fn, but no further tick is started.
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
