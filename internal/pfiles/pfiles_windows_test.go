//go:build windows

package pfiles

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v (%s)", err, out)
	}
}

// Windows handle semantics, verified empirically on the CI runner:
// while the target directory's handle is open (which is exactly
// the window between pfiles' verification and its handle-based
// rename), renaming an ANCESTOR of the target fails with Access
// Denied — the namespace above the verified object is frozen by
// the open handle, for us and for any other process alike. This
// test pins that platform guarantee as a canary: phase 1 asserts
// the ancestor swap CANNOT be staged mid-move and that the move
// completes on the verified object; phase 2 then stages the swap
// legitimately (between move and restore, when the original parent
// holds no open handles) and asserts the restore fails closed.
func TestHookSwapParentToJunctionDuringRename(t *testing.T) {
	profiles := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(profiles, "p", "prof")
	writeFile(t, filepath.Join(target, "Preferences"), "user-data")

	parked := filepath.Join(profiles, "p-parked")
	var renameErr error
	HookBeforeRename = func() {
		renameErr = os.Rename(filepath.Join(profiles, "p"), parked)
	}
	defer func() { HookBeforeRename = nil }()

	q, err := MoveToQuarantine(profiles, target, ".q-swap")
	if err != nil {
		t.Fatalf("MoveToQuarantine: %v", err)
	}
	defer func() { _ = q.Close() }()
	HookBeforeRename = nil
	if renameErr == nil {
		t.Fatal("ancestor rename unexpectedly succeeded while the target handle was open; " +
			"the Windows namespace-freeze guarantee this design relies on has changed — re-audit required")
	}
	if _, err := os.Stat(filepath.Join(q.Path(), "Preferences")); err != nil {
		t.Errorf("the verified object was not the one moved: %v", err)
	}

	// Phase 2: the profile now sits in quarantine and the original
	// parent holds no open handles, so the swap CAN be staged.
	if err := os.Rename(filepath.Join(profiles, "p"), parked); err != nil {
		t.Fatalf("stage phase-2 swap (rename parent): %v", err)
	}
	makeJunction(t, filepath.Join(profiles, "p"), outside)

	err = q.Restore()
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Restore through swapped parent: err = %v, want ErrUnsafe (fail closed)", err)
	}
	if _, err := os.Stat(filepath.Join(q.Path(), "Preferences")); err != nil {
		t.Errorf("profile should still be safe in quarantine after refused restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof")); !os.IsNotExist(err) {
		t.Errorf("profile was restored through the junction (err=%v)", err)
	}
	// Cleanup: drop the junction (removes the link only), restore
	// the parked parent.
	_ = os.Remove(filepath.Join(profiles, "p"))
	_ = os.Rename(parked, filepath.Join(profiles, "p"))
}

// Post-verification state: an intermediate component has ALREADY
// been replaced by a junction whose target holds a same-named
// directory. The move must refuse — the parent is a reparse point
// and the object behind the path is not the verified one.
func TestIntermediateJunctionBeforeMoveRefused(t *testing.T) {
	profiles := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "prof", "Preferences"), "outside-data")
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	mid := filepath.Join(profiles, "mid")
	writeFile(t, filepath.Join(mid, "prof", "Preferences"), "user-data")
	parked := filepath.Join(profiles, "mid-parked")
	if err := os.Rename(mid, parked); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, mid, outside)

	_, err := MoveToQuarantine(profiles, filepath.Join(mid, "prof"), ".q-j")
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof", "Preferences")); err != nil {
		t.Errorf("outside profile was moved or harmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parked, "prof", "Preferences")); err != nil {
		t.Errorf("real profile was touched: %v", err)
	}
}

// Wipe boundary: after the move, an attacker tries to replace
// the profiles root path with a junction to an outside directory
// holding an impostor quarantine slot and a sentinel — BEFORE the
// verified handle is released. Two platform outcomes are possible
// and both are safe:
//
//   - Windows freezes the namespace above an open object handle,
//     so the root rename itself fails (the same semantics the
//     rename-window canary pins). The swap cannot be staged at
//     all; the wipe then proceeds on the verified object.
//   - If a platform allowed the rename, the junction+impostor
//     staging completes — and the wipe, driven by the handle, must
//     still delete ONLY the original object (now parked) and leave
//     the impostor and sentinel untouched.
//
// Either way the test fails if outside data is harmed.
func TestWipeAfterProfilesRootSwapAttempt(t *testing.T) {
	base := t.TempDir()
	profiles := filepath.Join(base, "profiles")
	outside := t.TempDir()
	target := filepath.Join(profiles, "prof")
	writeFile(t, filepath.Join(target, "Preferences"), "user-data")
	const slot = ".q-wipe"
	writeFile(t, filepath.Join(outside, slot, "sentinel.txt"), "impostor")
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")

	q, err := MoveToQuarantine(profiles, target, slot)
	if err != nil {
		t.Fatalf("MoveToQuarantine: %v", err)
	}
	defer func() { _ = q.Close() }()

	parked := filepath.Join(base, "profiles-real")
	swapStaged := false
	if err := os.Rename(profiles, parked); err == nil {
		// Namespace allowed the rename: complete the attack
		// staging with a junction at the original root path.
		cmd := exec.Command("cmd", "/c", "mklink", "/J", profiles, outside)
		if out, err := cmd.CombinedOutput(); err == nil {
			swapStaged = true
		} else {
			t.Logf("mklink after root rename failed: %v (%s)", err, out)
		}
	} else {
		t.Logf("profiles root rename blocked while the quarantine handle is open (namespace freeze): %v", err)
	}

	if err := q.Wipe(); err != nil {
		t.Fatalf("Wipe after root swap attempt: %v", err)
	}
	// The root's deletion completes when its verified handle is
	// closed; release it before probing the namespace.
	if err := q.Close(); err != nil {
		t.Fatalf("Close after wipe: %v", err)
	}
	// The ORIGINAL object must be gone, wherever it lives now.
	origSlot := filepath.Join(profiles, slot)
	if swapStaged {
		origSlot = filepath.Join(parked, slot)
	}
	if _, err := os.Stat(origSlot); !os.IsNotExist(err) {
		t.Errorf("original quarantine object was not wiped (err=%v)", err)
	}
	if swapStaged {
		if _, err := os.Stat(filepath.Join(outside, slot, "sentinel.txt")); err != nil {
			t.Errorf("impostor quarantine at the junction target was wiped: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
}

// A wipe that cannot complete (a locked file inside the
// quarantine) fails with an error naming the quarantine path, and
// the data is retained for a successful retry — never silently
// lost.
func TestWipeFailureReportsQuarantinePath(t *testing.T) {
	profiles := t.TempDir()
	target := filepath.Join(profiles, "prof")
	lockedRel := filepath.Join("sub", "locked.txt")
	writeFile(t, filepath.Join(target, lockedRel), "user-data")

	q, err := MoveToQuarantine(profiles, target, ".q-lock")
	if err != nil {
		t.Fatalf("MoveToQuarantine: %v", err)
	}
	defer func() { _ = q.Close() }()

	lockedPath := filepath.Join(q.Path(), lockedRel)
	lockPtr, err := syscall.UTF16PtrFromString(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	lockHandle, err := syscall.CreateFile(lockPtr, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("lock file: %v", err)
	}
	err = q.Wipe()
	_ = syscall.CloseHandle(lockHandle)
	if err == nil {
		t.Fatal("wipe with a locked file succeeded; want a reported failure")
	}
	if !strings.Contains(err.Error(), q.Path()) {
		t.Errorf("wipe failure must name the residual quarantine path %s, got: %v", q.Path(), err)
	}
	if _, serr := os.Stat(lockedPath); serr != nil {
		t.Errorf("quarantine data was silently lost after the failed wipe: %v", serr)
	}
	if err := q.Wipe(); err != nil {
		t.Fatalf("retry after unlock: %v", err)
	}
	// Deletion of the root completes when its handle closes.
	if err := q.Close(); err != nil {
		t.Fatalf("Close after retry wipe: %v", err)
	}
	if _, err := os.Stat(q.Path()); !os.IsNotExist(err) {
		t.Errorf("quarantine still exists after successful retry (err=%v)", err)
	}
}

// The profiles root itself is a junction: refuse.
func TestProfilesRootJunctionRefused(t *testing.T) {
	base := t.TempDir()
	realProfiles := filepath.Join(base, "profiles-real")
	writeFile(t, filepath.Join(realProfiles, "prof", "Preferences"), "user-data")
	linkRoot := filepath.Join(base, "profiles")
	makeJunction(t, linkRoot, realProfiles)
	_, err := MoveToQuarantine(linkRoot, filepath.Join(linkRoot, "prof"), ".q-1")
	if err == nil {
		t.Fatal("move through a junction profiles root was allowed")
	}
	if _, err := os.Stat(filepath.Join(realProfiles, "prof", "Preferences")); err != nil {
		t.Errorf("real profile was touched: %v", err)
	}
}
