//go:build windows

package capture

import "testing"

// The hook lifecycle is serialized: a second start while one
// capture exists is refused (never a silent handler swap), stop
// waits for the thread to exit, and start/stop can repeat. The
// test skips only when the environment cannot install hooks at
// all (for example a non-interactive session) — the first
// StartCapture error is the skip signal.
func TestCaptureLifecycleSerialization(t *testing.T) {
	h := func(Event) {}
	if err := StartCapture(h); err != nil {
		t.Skipf("hooks unavailable in this environment: %v", err)
	}
	if err := StartCapture(h); err != ErrBusy {
		t.Fatalf("second StartCapture = %v, want ErrBusy", err)
	}
	StopCapture()
	// After StopCapture returns, the thread is gone: a new
	// capture may start immediately.
	if err := StartCapture(h); err != nil {
		t.Fatalf("StartCapture after stop = %v, want nil", err)
	}
	StopCapture()
	StopCapture() // idempotent
	if err := StartCapture(h); err != nil {
		t.Fatalf("StartCapture after double stop = %v, want nil", err)
	}
	StopCapture()
}
