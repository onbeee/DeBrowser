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
