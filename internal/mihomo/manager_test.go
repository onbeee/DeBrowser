package mihomo

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
)

// fakeChild is a childProcess test double: Wait blocks until the
// test closes waitRelease (or forever, when never released), Kill
// records the call and returns killErr.
type fakeChild struct {
	waitErr     error
	killErr     error
	waitRelease chan struct{}
	killed      chan struct{}
}

func newFakeChild() *fakeChild {
	return &fakeChild{waitRelease: make(chan struct{}), killed: make(chan struct{})}
}

func (f *fakeChild) Wait() error {
	<-f.waitRelease
	return f.waitErr
}

func (f *fakeChild) Kill() error {
	select {
	case <-f.killed:
	default:
		close(f.killed)
	}
	return f.killErr
}

func managerWithChild(child childProcess, stopTimeout time.Duration) (*Manager, chan struct{}) {
	m := &Manager{stopTimeout: stopTimeout}
	done := make(chan struct{})
	m.child = child
	m.done = done
	go func() {
		_ = child.Wait()
		close(done)
	}()
	return m, done
}

func TestManagerStopSuccess(t *testing.T) {
	child := newFakeChild()
	m, _ := managerWithChild(child, 5*time.Second)
	if !m.Running() {
		t.Fatal("manager must report running with a live child")
	}
	go func() {
		<-child.killed
		close(child.waitRelease)
	}()
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.Running() {
		t.Fatal("manager must not report running after a successful stop")
	}
	// Stopping again is a no-op.
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestManagerStopKillFailureKeepsHandle(t *testing.T) {
	child := newFakeChild()
	child.killErr = errors.New("kill denied")
	m, _ := managerWithChild(child, 5*time.Second)
	if err := m.Stop(); err == nil {
		t.Fatal("Stop must return the kill error, not report success")
	}
	if !m.Running() {
		t.Fatal("the child handle must be kept when the stop failed")
	}
	close(child.waitRelease) // let the waiter goroutine finish
}

func TestManagerStopTimeoutKeepsHandle(t *testing.T) {
	child := newFakeChild() // Wait is never released: the process does not exit
	m, _ := managerWithChild(child, 150*time.Millisecond)
	start := time.Now()
	if err := m.Stop(); err == nil {
		t.Fatal("Stop must time out with an error when the process does not exit")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Stop took %v, want roughly the configured timeout", elapsed)
	}
	if !m.Running() {
		t.Fatal("the child handle must be kept after a stop timeout")
	}
	close(child.waitRelease)
}

func TestManagerStopWithoutChild(t *testing.T) {
	m := &Manager{stopTimeout: time.Second}
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop with nothing started: %v", err)
	}
	if m.Running() {
		t.Fatal("a manager that never started must not report running")
	}
}

// TestEnsureBinaryVerifiesExistingFile: a file sitting at the
// core path that is NOT the official pinned binary must be
// refused, never executed or silently trusted.
func TestEnsureBinaryVerifiesExistingFile(t *testing.T) {
	if _, ok := releaseAssets[runtime.GOOS+"/"+runtime.GOARCH]; !ok {
		t.Skip("no pinned core asset for this platform")
	}
	dir := t.TempDir()
	bin := BinaryPath(dir)
	if err := os.WriteFile(bin, []byte("not the official core"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBinary(context.Background(), dir); err == nil {
		t.Fatal("a foreign file at the core path must be refused")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal("the refused file must be left in place for the user to remove")
	}
	// The genuine pinned binary, when provided, is accepted.
	genuine := os.Getenv("DEBROWSER_TEST_MIHOMO")
	if genuine == "" {
		t.Skip("DEBROWSER_TEST_MIHOMO not set; genuine-binary acceptance not checked")
	}
	data, err := os.ReadFile(genuine)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o750); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureBinary(context.Background(), dir)
	if err != nil {
		t.Fatalf("genuine binary refused: %v", err)
	}
	if got != bin {
		t.Fatalf("EnsureBinary = %q, want %q", got, bin)
	}
}

// ---------------------------------------------------------------------------
// Real-binary startup cleanup (DEBROWSER_TEST_MIHOMO gated)
// ---------------------------------------------------------------------------

// startCleanupFixture builds a manager whose client polls a
// controller port the core is NOT listening on (the core's own
// config uses a different one), so readiness can never succeed
// while the core itself runs fine. It returns the manager and
// the ports the core actually occupies.
func startCleanupFixture(t *testing.T, readyTimeout time.Duration) (*Manager, int, int) {
	t.Helper()
	bin := os.Getenv("DEBROWSER_TEST_MIHOMO")
	if bin == "" {
		t.Skip("DEBROWSER_TEST_MIHOMO not set; real-core tests skipped")
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(BinaryPath(dir), data, 0o750); err != nil {
		t.Fatal(err)
	}
	const corePort, wrongPort, listenerPort = 19099, 19199, 19101
	cfg := "log-level: warning\nmode: rule\n" +
		"external-controller: \"127.0.0.1:19099\"\nsecret: \"test-secret\"\n" +
		"listeners:\n  - {name: l1, type: mixed, listen: 127.0.0.1, port: 19101}\n" +
		"rules:\n  - \"MATCH,DIRECT\"\n"
	cfgPath := dir + "/config.yaml"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o640); err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir, cfgPath, NewClient(wrongPort, "test-secret"), wrongPort)
	m.readyTimeout = readyTimeout
	return m, corePort, listenerPort
}

func assertPortFree(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !ProbePort(port) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %d is still occupied; a core process was left behind", port)
}

// A readiness timeout must terminate the just-started core: no
// process, no controller, no listener survives a failed Start.
func TestManagerStartTimeoutLeavesNoProcess(t *testing.T) {
	m, corePort, listenerPort := startCleanupFixture(t, 800*time.Millisecond)
	if err := m.Start(context.Background()); err == nil {
		t.Fatal("start with an unreachable controller must fail")
	}
	if m.Running() {
		t.Fatal("manager still reports running after a failed start")
	}
	assertPortFree(t, corePort)
	assertPortFree(t, listenerPort)
}

// Same guarantee when the caller's context is cancelled during
// the readiness wait.
func TestManagerStartContextCancelLeavesNoProcess(t *testing.T) {
	m, corePort, listenerPort := startCleanupFixture(t, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	if err := m.Start(ctx); err == nil {
		t.Fatal("start with a cancelled context must fail")
	}
	if m.Running() {
		t.Fatal("manager still reports running after a cancelled start")
	}
	assertPortFree(t, corePort)
	assertPortFree(t, listenerPort)
}

// If a failed start's child cannot be confirmed exited,
// abortStart must KEEP tracking it: clearing the handle would
// lose the process while it may still be running. Once the exit
// is confirmed, tracking is cleared.
func TestAbortStartKeepsTrackingWhenExitUnconfirmed(t *testing.T) {
	child := newFakeChild() // Wait blocks until waitRelease closes; Kill succeeds but the process "does not exit"
	m, done := managerWithChild(child, 150*time.Millisecond)
	m.abortStart(child, done)
	m.mu.Lock()
	tracked := m.child
	m.mu.Unlock()
	if tracked == nil {
		t.Fatal("abortStart cleared tracking although the child never exited")
	}
	if !m.Running() {
		t.Fatal("manager must still report the unconfirmed child as running")
	}
	// Let the child exit; a second abort clears the tracking.
	close(child.waitRelease)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child exit was not observed")
	}
	m.abortStart(child, done)
	m.mu.Lock()
	tracked = m.child
	m.mu.Unlock()
	if tracked != nil {
		t.Fatal("abortStart must clear tracking once the exit is confirmed")
	}
}
