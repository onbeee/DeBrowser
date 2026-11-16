//go:build !windows

package runner

import (
	"errors"
	"testing"
	"time"
)

func TestUnixLaunchTracksRealChild(t *testing.T) {
	l := unixLauncher{}
	proc, err := launchChild(t, l, 0)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if proc.PID() <= 0 || proc.CreatedAt() <= 0 {
		t.Fatalf("tracked identity incomplete: pid=%d created=%d", proc.PID(), proc.CreatedAt())
	}
	info, err := l.Inspect(proc.PID())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.CreatedAt != proc.CreatedAt() {
		t.Fatalf("inspect created = %d, want %d", info.CreatedAt, proc.CreatedAt())
	}
	killAndWait(t, proc)
}

// When the identity read right after Start fails, the child must
// be terminated exactly and the error must be an
// UntrackedChildError confirming the exit — never a plain
// failure that strands a running process.
func TestUnixLaunchIdentityFailureTerminatesChild(t *testing.T) {
	orig := procStatFn
	procStatFn = func(int) (int64, error) { return 0, errors.New("stat unavailable") }
	defer func() { procStatFn = orig }()

	l := unixLauncher{}
	_, err := launchChild(t, l, 0)
	var untracked *UntrackedChildError
	if !errors.As(err, &untracked) {
		t.Fatalf("launch error = %v, want UntrackedChildError", err)
	}
	if !untracked.ChildExited {
		t.Fatal("ChildExited = false, want confirmed termination")
	}
	if untracked.PID <= 0 {
		t.Fatalf("PID = %d, want the started child's pid", untracked.PID)
	}
	// The child is really gone (checked with the unstubbed read).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ierr := procStat(untracked.PID); errors.Is(ierr, ErrProcessGone) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d still exists after the untracked launch failure", untracked.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
