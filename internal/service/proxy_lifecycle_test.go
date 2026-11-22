package service

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Lifecycle-mutex tests (§ reliability round): instance start,
// stop and deletion and the proxy port reassignment share ONE
// per-instance lock, wired here exactly as main wires it. The
// tests drive the REAL exported operations against each other
// and assert the interleavings that must never happen.

type lifecycleFixture struct {
	instSvc  *InstanceService
	proxySvc *ProxyService
	fl       *fakeLauncher
	st       *store.Store
	instID   string
	oldPort  int
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	fl := newFakeLauncher()
	instSvc, cfg, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	inst, err := instSvc.CreateInstance("", CreateInstanceInput{Name: "并发实例", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	vault, _, err := secrets.Open(cfg.Layout.SecretsFile)
	if err != nil {
		t.Fatal(err)
	}
	proxySvc := NewProxyService(db, cfg.Layout, vault, slog.Default())
	// The main.go wiring, verbatim in shape.
	instSvc.ProxyPortFor = proxySvc.ProxyPortForInstance
	proxySvc.InstanceStatus = func(instanceID string) (string, bool) {
		got, err := instSvc.GetInstance(instanceID)
		if err != nil {
			return "", false
		}
		return string(got.Status), true
	}
	proxySvc.Lifecycle = instSvc.Lifecycle

	src, err := proxySvc.AddURLSource("op", "机场", testSubURL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := proxySvc.SetBinding("op", inst.ID, src.ID, "NodeA")
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleFixture{
		instSvc: instSvc, proxySvc: proxySvc, fl: fl,
		st: store.New(db), instID: inst.ID, oldPort: b.Port,
	}
}

func (f *lifecycleFixture) bindingPort(t *testing.T) int {
	t.Helper()
	b, err := f.st.GetBinding(t.Context(), f.instID)
	if err != nil {
		t.Fatal(err)
	}
	return b.Port
}

// Case 1: a start in flight (blocked inside its proxy gate,
// lifecycle lock held) makes a reassignment WAIT; once the
// start settles — here refused by the gate because the core is
// stopped, leaving the instance stopped — the reassignment
// re-checks the state and proceeds.
func TestLifecycleStartInFlightBlocksReassign(t *testing.T) {
	f := newLifecycleFixture(t)

	gateEntered := make(chan struct{})
	gateRelease := make(chan struct{})
	realGate := f.instSvc.ProxyPortFor
	var gateOnce atomic.Bool
	f.instSvc.ProxyPortFor = func(id string) (int, error) {
		if gateOnce.CompareAndSwap(false, true) {
			close(gateEntered)
			<-gateRelease
		}
		return realGate(id)
	}

	startDone := make(chan error, 1)
	go func() { startDone <- f.instSvc.StartInstance("op", f.instID) }()
	<-gateEntered // the start now holds the lifecycle lock inside its gate

	reassignDone := make(chan error, 1)
	go func() {
		_, err := f.proxySvc.ReassignInstancePort("op", f.instID)
		reassignDone <- err
	}()
	select {
	case err := <-reassignDone:
		t.Fatalf("reassignment completed (%v) while a start was in flight — the lock is not shared", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(gateRelease)
	if err := <-startDone; err == nil {
		t.Fatal("the start should be refused by the gate (core stopped)")
	}
	if err := <-reassignDone; err != nil {
		t.Fatalf("reassignment after the start settled: %v", err)
	}
	if got := f.bindingPort(t); got == f.oldPort {
		t.Fatal("the reassignment did not move the port")
	}
}

// Case 2: a reassignment in flight (blocked inside its
// under-lock status check) makes a start WAIT; the start can
// only proceed after the reassignment fully settled, so it
// reads the new, consistent port state — never a half-updated
// one.
func TestLifecycleReassignInFlightBlocksStart(t *testing.T) {
	f := newLifecycleFixture(t)

	statusEntered := make(chan struct{})
	statusRelease := make(chan struct{})
	realStatus := f.proxySvc.InstanceStatus
	var statusOnce atomic.Bool
	f.proxySvc.InstanceStatus = func(id string) (string, bool) {
		if statusOnce.CompareAndSwap(false, true) {
			close(statusEntered)
			<-statusRelease
		}
		return realStatus(id)
	}

	var reassignSettled atomic.Bool
	reassignDone := make(chan error, 1)
	go func() {
		_, err := f.proxySvc.ReassignInstancePort("op", f.instID)
		reassignSettled.Store(true)
		reassignDone <- err
	}()
	<-statusEntered // the reassignment now holds the lifecycle lock

	startDone := make(chan error, 1)
	go func() { startDone <- f.instSvc.StartInstance("op", f.instID) }()
	select {
	case err := <-startDone:
		t.Fatalf("start completed (%v) while a reassignment was in flight — the lock is not shared", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(statusRelease)
	if err := <-reassignDone; err != nil {
		t.Fatalf("reassignment: %v", err)
	}
	// The start now proceeds against the settled state; the
	// gate refuses it (core stopped), which is fine — what
	// matters is that it ran strictly after the reassignment.
	if err := <-startDone; err == nil {
		t.Fatal("the start should be refused by the gate (core stopped)")
	}
	if !reassignSettled.Load() {
		t.Fatal("the start finished before the reassignment settled")
	}
	if got := f.bindingPort(t); got == f.oldPort {
		t.Fatal("the reassignment did not move the port")
	}
}

// Cases 3–5: reassignment refuses every non-stopped state —
// starting, stopping, unknown — and a genuinely running
// instance; after a real stop it succeeds again.
func TestLifecycleReassignRefusesNonStoppedStates(t *testing.T) {
	f := newLifecycleFixture(t)

	setTransient := func(st model.InstanceStatus) {
		f.instSvc.rtMu.Lock()
		if st == "" {
			delete(f.instSvc.transient, f.instID)
		} else {
			f.instSvc.transient[f.instID] = st
		}
		f.instSvc.rtMu.Unlock()
	}
	for _, st := range []model.InstanceStatus{model.StatusStarting, model.StatusStopping} {
		setTransient(st)
		if _, err := f.proxySvc.ReassignInstancePort("op", f.instID); err == nil ||
			!strings.Contains(err.Error(), "请先停止实例") {
			t.Fatalf("state %s: err = %v, want the not-stopped refusal", st, err)
		}
	}
	setTransient("")

	f.instSvc.rtMu.Lock()
	f.instSvc.unverified[f.instID] = true
	f.instSvc.rtMu.Unlock()
	if _, err := f.proxySvc.ReassignInstancePort("op", f.instID); err == nil ||
		!strings.Contains(err.Error(), "请先停止实例") {
		t.Fatalf("state unknown: err = %v, want the not-stopped refusal", err)
	}
	f.instSvc.rtMu.Lock()
	delete(f.instSvc.unverified, f.instID)
	f.instSvc.rtMu.Unlock()

	// A real start (gate bypassed: this sub-case is about the
	// lifecycle state, not the proxy) → running → refused; a
	// real stop → stopped → allowed.
	f.instSvc.ProxyPortFor = nil
	if err := f.instSvc.StartInstance("op", f.instID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.proxySvc.ReassignInstancePort("op", f.instID); err == nil ||
		!strings.Contains(err.Error(), "请先停止实例") {
		t.Fatalf("state running: err = %v, want the not-stopped refusal", err)
	}
	if err := f.instSvc.StopInstance("op", f.instID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitStatus(t, f.instSvc, f.instID, model.StatusStopped)
	view, err := f.proxySvc.ReassignInstancePort("op", f.instID)
	if err != nil {
		t.Fatalf("reassignment after a real stop: %v", err)
	}
	if view.Port == f.oldPort {
		t.Fatal("the reassignment did not move the port")
	}
}
