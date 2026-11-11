//go:build linux || windows

package pfiles

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMoveRestoreWipeCycle(t *testing.T) {
	profiles := t.TempDir()
	target := filepath.Join(profiles, "prof")
	writeFile(t, filepath.Join(target, "sub", "Preferences"), "user-data")

	q, err := MoveToQuarantine(profiles, target, ".q-1")
	if err != nil {
		t.Fatalf("MoveToQuarantine: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still at original location (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(q.Path(), "sub", "Preferences")); err != nil {
		t.Errorf("content missing in quarantine: %v", err)
	}
	if err := q.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "sub", "Preferences")); err != nil {
		t.Errorf("content missing after restore: %v", err)
	}

	q2, err := MoveToQuarantine(profiles, target, ".q-2")
	if err != nil {
		t.Fatalf("second move: %v", err)
	}
	defer func() { _ = q2.Close() }()
	if err := q2.Wipe(); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if _, err := os.Stat(q2.Path()); !os.IsNotExist(err) {
		t.Errorf("quarantine still exists after wipe (err=%v)", err)
	}
	if _, err := os.Stat(profiles); err != nil {
		t.Errorf("profiles root was removed: %v", err)
	}
	_ = q.Close()
}

func TestMoveOccupiedSlotFailsClosed(t *testing.T) {
	profiles := t.TempDir()
	target := filepath.Join(profiles, "prof")
	writeFile(t, filepath.Join(target, "Preferences"), "user-data")
	// A racing process pre-created our quarantine slot, with content.
	writeFile(t, filepath.Join(profiles, ".q-taken", "sentinel.txt"), "not-yours")

	_, err := MoveToQuarantine(profiles, target, ".q-taken")
	if !errors.Is(err, ErrOccupied) {
		t.Fatalf("err = %v, want ErrOccupied", err)
	}
	if _, err := os.Stat(filepath.Join(profiles, ".q-taken", "sentinel.txt")); err != nil {
		t.Errorf("occupant was harmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "Preferences")); err != nil {
		t.Errorf("target was moved despite the collision: %v", err)
	}
}

func TestMoveRefusesProfilesRootRedirect(t *testing.T) {
	// Scenario: the profiles root itself is replaced by a link to
	// a real directory elsewhere. Anchoring must refuse rather than
	// operate inside the redirected tree.
	base := t.TempDir()
	realProfiles := filepath.Join(base, "profiles-real")
	writeFile(t, filepath.Join(realProfiles, "prof", "Preferences"), "user-data")
	linkRoot := filepath.Join(base, "profiles")
	if err := os.Symlink(realProfiles, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := MoveToQuarantine(linkRoot, filepath.Join(linkRoot, "prof"), ".q-1")
	if err == nil {
		t.Fatal("move through a redirected profiles root was allowed")
	}
	if _, err := os.Stat(filepath.Join(realProfiles, "prof", "Preferences")); err != nil {
		t.Errorf("real profile was touched: %v", err)
	}
}

func TestMoveRefusesSymlinkTarget(t *testing.T) {
	profiles := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	link := filepath.Join(profiles, "prof")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := MoveToQuarantine(profiles, link, ".q-1")
	if err == nil {
		t.Fatal("move of a symlink target was allowed")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside was touched: %v", err)
	}
}
