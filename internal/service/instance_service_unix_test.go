//go:build !windows

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Deterministic version of the round-4 attack at the service
// level: verification completes, THEN an intermediate component of
// the profile path is swapped for a symlink to an outside directory
// that contains a same-named directory. The anchored quarantine
// move must refuse; the record and both directories stay intact.
func TestPurgeSwapIntermediateAfterVerifyRefused(t *testing.T) {
	svc, cfg := testInstanceService(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "prof"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "prof", "Preferences"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile := func(p, content string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join(cfg.Layout.Profiles, "mid", "prof", "Preferences"), "user-data")
	in := seedInstance(t, cfg, "swap", "profiles/mid/prof")
	id := in.ID

	svc.afterVerifyHook = func() {
		_ = os.Rename(filepath.Join(cfg.Layout.Profiles, "mid"), filepath.Join(cfg.Layout.Profiles, "mid-parked"))
		_ = os.Symlink(outside, filepath.Join(cfg.Layout.Profiles, "mid"))
	}
	if err := svc.DeleteInstanceAndProfile(id); err == nil {
		t.Fatal("purge under post-verify component swap succeeded, want refusal")
	}
	if _, err := svc.GetInstance(id); err != nil {
		t.Errorf("record lost after refused purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof", "Preferences")); err != nil {
		t.Errorf("outside directory was harmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.Profiles, "mid-parked", "prof", "Preferences")); err != nil {
		t.Errorf("real profile was moved or harmed: %v", err)
	}
}

// The record deletion fails AND the profiles root's namespace
// entry is swapped before the restore: the fd-anchored restore
// still returns the profile into the pinned root (visible at the
// parked path), never through the attacker's symlink.
func TestPurgeRestoreWithSwappedRoot(t *testing.T) {
	svc, cfg := testInstanceService(t)
	outside := t.TempDir()
	created, err := svc.CreateInstance(CreateInstanceInput{Name: "恢复"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	profileDir := filepath.Join(cfg.Layout.Data, filepath.FromSlash(created.UserDataDir))
	marker := filepath.Join(profileDir, "Preferences")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("user-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	profilesRoot := cfg.Layout.Profiles
	parkedRoot := profilesRoot + "-parked"
	svc.deleteRecord = func(ctx context.Context, id string) error {
		return errors.New("injected database failure")
	}
	svc.beforeRestoreHook = func() {
		_ = os.Rename(profilesRoot, parkedRoot)
		_ = os.Symlink(outside, profilesRoot)
	}
	if err := svc.DeleteInstanceAndProfile(created.ID); err == nil {
		t.Fatal("purge with failing record delete succeeded, want error")
	}
	restored := filepath.Join(parkedRoot, filepath.Base(profileDir), "Preferences")
	if _, err := os.Stat(restored); err != nil {
		t.Errorf("profile was not restored into the pinned profiles root: %v", err)
	}
	if _, err := svc.GetInstance(created.ID); err != nil {
		t.Errorf("record lost after aborted purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, filepath.Base(profileDir))); !os.IsNotExist(err) {
		t.Errorf("restore went through the attacker's symlink (err=%v)", err)
	}
}
