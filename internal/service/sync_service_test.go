package service

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/capture"
	"github.com/onbeee/DeBrowser/internal/model"
)

// syncStartFixture builds a SyncService whose master and
// follower instances are really "running" (fake launcher) with
// their CDP ports redirected at fake CDP endpoints, and whose
// input capture is a counting fake — so session lifecycle tests
// verify actual session and resource state, not error strings.
type syncStartFixture struct {
	svc      *SyncService
	isvc     *InstanceService
	master   model.Instance
	follower model.Instance
	mCDP     *recordingCDP
	fCDP     *recordingCDP

	mu            sync.Mutex
	captureStarts int
	captureStops  int
}

func newSyncStartFixture(t *testing.T) *syncStartFixture {
	t.Helper()
	_, isvc, _ := bookmarkTestServices(t)
	fx := &syncStartFixture{isvc: isvc}
	fx.svc = NewSyncService(isvc)
	fx.svc.captureStart = func(h capture.Handler) error {
		fx.mu.Lock()
		fx.captureStarts++
		fx.mu.Unlock()
		return nil
	}
	fx.svc.captureStop = func() {
		fx.mu.Lock()
		fx.captureStops++
		fx.mu.Unlock()
	}
	fx.master = makeInstance(t, isvc, "sync-master")
	fx.follower = makeInstance(t, isvc, "sync-follower")
	if err := isvc.StartInstance("", fx.master.ID); err != nil {
		t.Fatal(err)
	}
	if err := isvc.StartInstance("", fx.follower.ID); err != nil {
		t.Fatal(err)
	}
	fx.mCDP = newRecordingCDP(t)
	fx.fCDP = newRecordingCDP(t)
	fx.setPort(fx.master.ID, fx.mCDP.port)
	fx.setPort(fx.follower.ID, fx.fCDP.port)
	t.Cleanup(func() {
		_ = fx.svc.StopSync("")
		_ = isvc.StopInstance("", fx.master.ID)
		_ = isvc.StopInstance("", fx.follower.ID)
	})
	return fx
}

func (fx *syncStartFixture) setPort(id string, port int) {
	fx.isvc.rtMu.Lock()
	defer fx.isvc.rtMu.Unlock()
	if w, ok := fx.isvc.procs[id]; ok {
		w.cdpPort = port
	}
}

func (fx *syncStartFixture) captureCounts() (int, int) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return fx.captureStarts, fx.captureStops
}

func (fx *syncStartFixture) start(t *testing.T) {
	t.Helper()
	if err := fx.svc.StartSync("", fx.master.ID, []string{fx.follower.ID}); err != nil {
		t.Fatal(err)
	}
}

// Concurrent StartSync calls: exactly one builds a session;
// the others get a clear refusal; capture starts exactly once.
func TestStartSyncConcurrentOnlyOneSession(t *testing.T) {
	fx := newSyncStartFixture(t)
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = fx.svc.StartSync("", fx.master.ID, []string{fx.follower.ID})
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !strings.Contains(err.Error(), "正在启动或进行中") {
			t.Fatalf("concurrent start error = %v, want the in-progress refusal", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1", succeeded)
	}
	st, err := fx.svc.GetSyncState("")
	if err != nil || st.Phase != "running" || len(st.Followers) != 1 {
		t.Fatalf("state = %+v, %v; want one running session with one follower", st, err)
	}
	if starts, _ := fx.captureCounts(); starts != 1 {
		t.Fatalf("capture starts = %d, want 1 (no overwritten capture handler)", starts)
	}
	if err := fx.svc.StopSync(""); err != nil {
		t.Fatal(err)
	}
	if starts, stops := fx.captureCounts(); starts != 1 || stops != 1 {
		t.Fatalf("capture starts/stops = %d/%d, want 1/1 after stop", starts, stops)
	}
}

// A failed start leaves nothing behind: no session, no capture,
// and a later start succeeds.
func TestStartSyncFailureThenRetry(t *testing.T) {
	fx := newSyncStartFixture(t)
	fx.setPort(fx.follower.ID, 1) // nothing listens there
	if err := fx.svc.StartSync("", fx.master.ID, []string{fx.follower.ID}); err == nil {
		t.Fatal("start with an unreachable follower must fail")
	}
	st, _ := fx.svc.GetSyncState("")
	if st.Phase != "idle" {
		t.Fatalf("phase = %q after failed start, want idle", st.Phase)
	}
	if starts, stops := fx.captureCounts(); starts != 0 || stops != 0 {
		t.Fatalf("capture starts/stops = %d/%d after failed start, want 0/0", starts, stops)
	}
	fx.setPort(fx.follower.ID, fx.fCDP.port)
	fx.start(t)
	st, _ = fx.svc.GetSyncState("")
	if st.Phase != "running" {
		t.Fatalf("phase = %q after retry, want running", st.Phase)
	}
}

// The master disconnecting right after the session starts ends
// the session through the monitor — the session must not linger
// as current, and capture must be released exactly once.
func TestStartSyncMasterDisconnectEndsSession(t *testing.T) {
	fx := newSyncStartFixture(t)
	fx.start(t)
	fx.mCDP.dropAll()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := fx.svc.GetSyncState("")
		if st.Phase == "idle" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := fx.svc.GetSyncState("")
	if st.Phase != "idle" {
		t.Fatalf("phase = %q after master disconnect, want idle", st.Phase)
	}
	if starts, stops := fx.captureCounts(); starts != 1 || stops != 1 {
		t.Fatalf("capture starts/stops = %d/%d, want 1/1", starts, stops)
	}
	// And a fresh session can start afterwards (no residue).
	fx.start(t)
	if err := fx.svc.StopSync(""); err != nil {
		t.Fatal(err)
	}
}

// Start/stop interleaving leaves no residual workers or capture
// handlers: counts pair up exactly, and state returns to idle.
func TestStartStopInterleaveNoResidue(t *testing.T) {
	fx := newSyncStartFixture(t)
	for i := 0; i < 5; i++ {
		fx.start(t)
		if err := fx.svc.StopSync(""); err != nil {
			t.Fatal(err)
		}
	}
	if starts, stops := fx.captureCounts(); starts != 5 || stops != 5 {
		t.Fatalf("capture starts/stops = %d/%d, want 5/5", starts, stops)
	}
	st, _ := fx.svc.GetSyncState("")
	if st.Phase != "idle" || len(st.Followers) != 0 {
		t.Fatalf("state = %+v, want idle with no followers", st)
	}
}
