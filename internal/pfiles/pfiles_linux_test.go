//go:build linux

package pfiles

import (
	"os"
	"path/filepath"
	"testing"
)

// Deterministic race: at the exact moment of the rename, an
// intermediate component of the target path is swapped for a
// symlink to an outside directory. A path-based rename would move
// the OUTSIDE directory; the fd-anchored renameat2 must move the
// pinned object — the outside tree stays untouched, and a restore
// lands back inside the pinned parent (visible at its parked path
// after the swap).
func TestHookSwapIntermediateDuringRename(t *testing.T) {
	profiles := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(profiles, "a", "b", "prof")
	writeFile(t, filepath.Join(target, "Preferences"), "user-data")

	parked := filepath.Join(profiles, "a-parked")
	HookBeforeRename = func() {
		_ = os.Rename(filepath.Join(profiles, "a"), parked)
		_ = os.Symlink(outside, filepath.Join(profiles, "a"))
	}
	defer func() { HookBeforeRename = nil }()

	q, err := MoveToQuarantine(profiles, target, ".q-swap")
	if err != nil {
		t.Fatalf("MoveToQuarantine under swap: %v", err)
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

	// Restore with the namespace still swapped: the object must
	// land in the PINNED parent directory (now reachable as
	// a-parked/b/prof), not through the attacker's symlink.
	if err := q.Restore(); err != nil {
		t.Fatalf("Restore under swap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parked, "b", "prof", "Preferences")); err != nil {
		t.Errorf("restored object did not land in the pinned parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof")); !os.IsNotExist(err) {
		t.Errorf("restore went through the symlink (err=%v)", err)
	}
}

// The quarantine slot being created by a racer BETWEEN our descent
// and the rename: renameat2 NOREPLACE reports ErrOccupied and the
// occupant is untouched.
func TestHookCreatesQuarantineSlotDuringRename(t *testing.T) {
	profiles := t.TempDir()
	target := filepath.Join(profiles, "prof")
	writeFile(t, filepath.Join(target, "Preferences"), "user-data")
	HookBeforeRename = func() {
		_ = os.MkdirAll(filepath.Join(profiles, ".q-race"), 0o755)
		_ = os.WriteFile(filepath.Join(profiles, ".q-race", "sentinel.txt"), []byte("racer"), 0o644)
	}
	defer func() { HookBeforeRename = nil }()

	_, err := MoveToQuarantine(profiles, target, ".q-race")
	if err == nil {
		t.Fatal("move into a slot created mid-rename was allowed")
	}
	if _, err := os.Stat(filepath.Join(profiles, ".q-race", "sentinel.txt")); err != nil {
		t.Errorf("racer-created slot was harmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "Preferences")); err != nil {
		t.Errorf("target moved despite collision: %v", err)
	}
}
