//go:build windows

package uia

import (
	"errors"
	"testing"
)

// Regression test for the StartSync panic: a SUCCESSFUL UIA job
// stores a nil error interface in the result pair, and reading it
// back with a direct .(error) assertion panics with "interface
// conversion: interface is nil, not error". runJob must return
// (value, nil) for a successful job and pass a real error through
// untouched. The job functions ignore the automation object, so
// the test does not depend on any real browser window.
func TestRunJobSuccessAndFailure(t *testing.T) {
	v, err := runJob(func(a *automation) (int, error) { return 42, nil })
	if err != nil {
		t.Fatalf("successful job returned error %v", err)
	}
	if v != 42 {
		t.Fatalf("successful job value = %d, want 42", v)
	}
	boom := errors.New("boom")
	_, err = runJob(func(a *automation) (int, error) { return 0, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("failing job error = %v, want the job's own error", err)
	}
}
