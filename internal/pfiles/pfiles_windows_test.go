//go:build windows

package pfiles

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v (%s)", err, out)
	}
}

// Deterministic race, Windows edition: at the exact moment of the
// rename, the target's parent is swapped for a junction to an
// outside directory. The handle-based rename must still move the
// VERIFIED object; the outside tree stays untouched. The restore
// afterwards must FAIL CLOSED: the parent path now resolves
// outside, so the object stays in quarantine rather than being
// renamed through the junction.
func TestHookSwapParentToJunctionDuringRename(t *testing.T) {
	profiles := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(profiles, "p", "prof")
	writeFile(t, filepath.Join(target, "Preferences"), "user-data")

	parked := filepath.Join(profiles, "p-parked")
	HookBeforeRename = func() {
		_ = os.Rename(filepath.Join(profiles, "p"), parked)
		cmd := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(profiles, "p"), outside)
		_ = cmd.Run()
	}
	defer func() { HookBeforeRename = nil }()

	q, err := MoveToQuarantine(profiles, target, ".q-swap")
	if err != nil {
		t.Fatalf("MoveToQuarantine under junction swap: %v", err)
	}
	defer func() { _ = q.Close() }()
	HookBeforeRename = nil

	if _, err := os.Stat(filepath.Join(q.Path(), "Preferences")); err != nil {
		t.Errorf("the verified object was not the one moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof")); !os.IsNotExist(err) {
		t.Errorf("an outside directory was moved into quarantine (err=%v)", err)
	}

	err = q.Restore()
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("Restore through swapped parent: err = %v, want ErrUnsafe (fail closed)", err)
	}
	if _, err := os.Stat(filepath.Join(q.Path(), "Preferences")); err != nil {
		t.Errorf("profile should still be safe in quarantine after refused restore: %v", err)
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
