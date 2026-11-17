package service

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/model"
)

// The bookmark hold and the instance lifecycle must be mutually
// exclusive: while a bookmark operation holds an instance, start
// and stop refuse it; while a start/stop transition is in flight,
// no bookmark operation can acquire it; and two bookmark
// operations never hold the same instance at once.

func TestBookmarkHoldBlocksStart(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "hold-start", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, gate, release, err := isvc.AcquireBookmarkAccess(inst.ID)
	if err != nil || gate != BookmarkGateStopped || release == nil {
		t.Fatalf("acquire = gate %q err %v", gate, err)
	}
	// A second bookmark operation on the same instance is busy.
	if _, _, g2, r2, _ := isvc.AcquireBookmarkAccess(inst.ID); g2 != BookmarkGateBusy || r2 != nil {
		t.Fatalf("second acquire = gate %q, want busy", g2)
	}
	// Start is refused while the hold lasts.
	if err := isvc.StartInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "书签操作") {
		t.Fatalf("start during hold = %v, want bookmark-operation refusal", err)
	}
	release()
	release() // idempotent
	// After release the start proceeds.
	if err := isvc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start after release: %v", err)
	}
	if st := statusOf(t, isvc, inst.ID); st != model.StatusRunning {
		t.Fatalf("status = %q, want running", st)
	}
	// Cleanup.
	if err := isvc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestBookmarkHoldBlocksStop(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "hold-stop", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := isvc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	// The public flow can never produce "running + held" (the
	// hold requires a stopped instance) — the stop-side check is
	// the guard for exactly that interleaving, so plant the hold
	// the way a racing acquirer would have set it.
	isvc.rtMu.Lock()
	isvc.bookmarkHolds[inst.ID] = true
	isvc.rtMu.Unlock()
	if err := isvc.StopInstance("", inst.ID); err == nil || !strings.Contains(err.Error(), "书签操作") {
		t.Fatalf("stop during hold = %v, want bookmark-operation refusal", err)
	}
	isvc.rtMu.Lock()
	delete(isvc.bookmarkHolds, inst.ID)
	isvc.rtMu.Unlock()
	if err := isvc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("stop after hold cleared: %v", err)
	}
}

func TestBookmarkAcquireRejectedDuringTransitions(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	for _, tr := range []model.InstanceStatus{model.StatusStarting, model.StatusStopping} {
		inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "transient-" + string(tr), BrowserID: "reg-a"})
		if err != nil {
			t.Fatal(err)
		}
		// Plant the in-flight transition exactly as a blocked
		// start/stop presents it to every consumer.
		isvc.rtMu.Lock()
		isvc.transient[inst.ID] = tr
		isvc.rtMu.Unlock()
		if _, _, gate, release, err := isvc.AcquireBookmarkAccess(inst.ID); err != nil || gate != BookmarkGateBusy || release != nil {
			t.Fatalf("acquire during %s = gate %q err %v, want busy", tr, gate, err)
		}
		isvc.rtMu.Lock()
		delete(isvc.transient, inst.ID)
		isvc.rtMu.Unlock()
		// Transition over: the stopped instance acquires cleanly.
		_, _, gate, release, err := isvc.AcquireBookmarkAccess(inst.ID)
		if err != nil || gate != BookmarkGateStopped {
			t.Fatalf("acquire after %s = gate %q err %v", tr, gate, err)
		}
		release()
	}
}

func TestBookmarkAcquireRejectedDuringRealStop(t *testing.T) {
	fl := newFakeLauncher()
	fl.closeExitsOnLaunch = true
	fl.closeDelayOnLaunch = 700 * time.Millisecond
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "real-stop", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- svc.StopInstance("", inst.ID) }()
	// Wait until the stop transition is actually in flight.
	deadline := time.Now().Add(3 * time.Second)
	for {
		svc.rtMu.Lock()
		tr := svc.transient[inst.ID]
		svc.rtMu.Unlock()
		if tr == model.StatusStopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stop transition never began")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, gate, release, err := svc.AcquireBookmarkAccess(inst.ID); err != nil || gate != BookmarkGateBusy || release != nil {
		t.Fatalf("acquire during real stop = gate %q err %v, want busy", gate, err)
	}
	if err := <-stopDone; err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Fully stopped now: acquisition succeeds again (no stale
	// hold, no stuck transition).
	_, _, gate, release, err := svc.AcquireBookmarkAccess(inst.ID)
	if err != nil || gate != BookmarkGateStopped {
		t.Fatalf("acquire after stop = gate %q err %v", gate, err)
	}
	release()
}

func TestConcurrentBookmarkAcquireSingleWinner(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "race-acquire", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	const racers = 16
	gates := make([]string, racers)
	releases := make([]func(), racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, g, r, err := isvc.AcquireBookmarkAccess(inst.ID)
			if err != nil {
				g = "error:" + err.Error()
			}
			gates[i] = g
			releases[i] = r
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	var winnerRelease func()
	for i := range gates {
		switch gates[i] {
		case BookmarkGateStopped:
			winners++
			winnerRelease = releases[i]
		case BookmarkGateBusy:
			if releases[i] != nil {
				t.Fatal("busy acquire returned a release function")
			}
		default:
			t.Fatalf("unexpected gate %q", gates[i])
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	winnerRelease()
	// The hold was released: a fresh acquire succeeds.
	_, _, gate, release, err := isvc.AcquireBookmarkAccess(inst.ID)
	if err != nil || gate != BookmarkGateStopped {
		t.Fatalf("acquire after winner release = gate %q err %v", gate, err)
	}
	release()
}

func TestBookmarkHoldsArePerInstance(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	a, err := isvc.CreateInstance("", CreateInstanceInput{Name: "per-inst-a", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := isvc.CreateInstance("", CreateInstanceInput{Name: "per-inst-b", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, ga, ra, err := isvc.AcquireBookmarkAccess(a.ID)
	if err != nil || ga != BookmarkGateStopped {
		t.Fatalf("acquire A = %q %v", ga, err)
	}
	// B is unaffected by A's hold: it can be acquired AND,
	// after its own release, started while A stays held.
	_, _, gb, rb, err := isvc.AcquireBookmarkAccess(b.ID)
	if err != nil || gb != BookmarkGateStopped {
		t.Fatalf("acquire B while A held = %q %v", gb, err)
	}
	rb()
	if err := isvc.StartInstance("", b.ID); err != nil {
		t.Fatalf("start B while A held: %v", err)
	}
	if err := isvc.StartInstance("", a.ID); err == nil {
		t.Fatal("start A while A held succeeded, want refusal")
	}
	ra()
	if err := isvc.StopInstance("", b.ID); err != nil {
		t.Fatalf("stop B: %v", err)
	}
}

func TestFailedAcquireLeavesNoHold(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: "no-stale-hold", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := isvc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	// Running: acquire reports running and holds nothing.
	if _, _, gate, release, err := isvc.AcquireBookmarkAccess(inst.ID); err != nil || gate != BookmarkGateRunning || release != nil {
		t.Fatalf("acquire while running = gate %q err %v", gate, err)
	}
	if err := isvc.StopInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, isvc, inst.ID, model.StatusStopped)
	// No stale hold from the failed acquire: acquisition and a
	// later start both work.
	_, _, gate, release, err := isvc.AcquireBookmarkAccess(inst.ID)
	if err != nil || gate != BookmarkGateStopped {
		t.Fatalf("acquire after stop = gate %q err %v", gate, err)
	}
	release()
	if err := isvc.StartInstance("", inst.ID); err != nil {
		t.Fatalf("start after failed-acquire cycle: %v", err)
	}
	if err := isvc.StopInstance("", inst.ID); err != nil {
		t.Fatalf("final stop: %v", err)
	}
}

func TestStartVersusBookmarkAcquireRace(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	for i := 0; i < 30; i++ {
		inst, err := isvc.CreateInstance("", CreateInstanceInput{
			Name: "start-race", BrowserID: "reg-a",
		})
		if err != nil {
			t.Fatal(err)
		}
		var startErr error
		var gate string
		var release func()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			startErr = isvc.StartInstance("", inst.ID)
		}()
		go func() {
			defer wg.Done()
			_, _, g, r, aerr := isvc.AcquireBookmarkAccess(inst.ID)
			if aerr != nil {
				g = "error"
			}
			gate, release = g, r
		}()
		wg.Wait()
		if gate == BookmarkGateStopped {
			// The bookmark operation won: the start MUST have
			// been refused, never run concurrently.
			if startErr == nil {
				t.Fatalf("iteration %d: bookmark hold acquired AND start succeeded — race window", i)
			}
			if !strings.Contains(startErr.Error(), "书签操作") {
				t.Fatalf("iteration %d: start error = %v, want bookmark refusal", i, startErr)
			}
			release()
		} else {
			// The start won (or is in flight): the acquire must
			// not have obtained the instance.
			if release != nil {
				t.Fatalf("iteration %d: gate %q with a release function", i, gate)
			}
			if startErr == nil {
				// Start fully succeeded; instance is running.
				// Clean up for the next iteration.
				_ = isvc.StopInstance("", inst.ID)
			}
		}
		// Retire the instance so the next iteration starts from
		// a clean stopped state with a unique name.
		if st := statusOf(t, isvc, inst.ID); st == model.StatusRunning {
			_ = isvc.StopInstance("", inst.ID)
		}
		if err := isvc.DeleteInstance("", inst.ID); err != nil {
			t.Fatalf("iteration %d cleanup delete: %v", i, err)
		}
	}
}
