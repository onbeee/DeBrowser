//go:build windows

package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Windows junction (reparse point) coverage for profile deletion.
// Junctions are created with mklink /J, which needs no administrator
// rights. These tests run on the Windows CI runner for real.

func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("mklink /J unavailable: %v (%s)", err, out)
	}
}

func TestPurgeRefusesJunctionEscapeOnWindows(t *testing.T) {
	svc, cfg := testInstanceService(t)
	outside := t.TempDir()
	inner := filepath.Join(outside, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(inner, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	// profiles/jmid is a junction to the OUTSIDE directory; the
	// stored profile path passes through it.
	makeJunction(t, filepath.Join(cfg.Layout.Profiles, "jmid"), outside)
	in := seedInstance(t, cfg, "Junction逃逸", "profiles/jmid/inner")
	err := svc.DeleteInstanceAndProfile(in.ID)
	if err == nil {
		t.Fatal("purge through junction to outside was allowed, want refusal")
	}
	if !strings.Contains(err.Error(), "拒绝删除") {
		t.Errorf("error = %v, want a 拒绝删除 refusal", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := svc.GetInstance(in.ID); err != nil {
		t.Errorf("metadata removed despite refusal: %v", err)
	}
}

func TestPurgeRefusesJunctionAsProfileDirOnWindows(t *testing.T) {
	svc, cfg := testInstanceService(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The profile directory itself is a junction to the outside.
	makeJunction(t, filepath.Join(cfg.Layout.Profiles, "jself"), outside)
	in := seedInstance(t, cfg, "Junction本体", "profiles/jself")
	if err := svc.DeleteInstanceAndProfile(in.ID); err == nil {
		t.Fatal("purge of junction profile dir was allowed, want refusal")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := svc.GetInstance(in.ID); err != nil {
		t.Errorf("metadata removed despite refusal: %v", err)
	}
	// Sanity: a normal profile still purges on Windows.
	normal, err := svc.CreateInstance(CreateInstanceInput{Name: "正常"})
	if err != nil {
		t.Fatalf("create normal: %v", err)
	}
	dir := filepath.Join(cfg.Layout.Data, filepath.FromSlash(normal.UserDataDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteInstanceAndProfile(normal.ID); err != nil {
		t.Fatalf("normal purge on Windows: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("normal profile dir still exists (err=%v)", err)
	}
}

// Deterministic round-4 attack, service level, real junction:
// verification completes, THEN the intermediate component is
// swapped for a junction to an outside directory holding a
// same-named directory. The purge must refuse; record, real
// profile and outside data all stay intact.
func TestPurgeSwapIntermediateJunctionAfterVerifyRefusedOnWindows(t *testing.T) {
	svc, cfg := testInstanceService(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "prof"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "prof", "Preferences"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	profDir := filepath.Join(cfg.Layout.Profiles, "mid", "prof")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profDir, "Preferences"), []byte("user-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := seedInstance(t, cfg, "junctionswap", "profiles/mid/prof")

	svc.afterVerifyHook = func() {
		_ = os.Rename(filepath.Join(cfg.Layout.Profiles, "mid"), filepath.Join(cfg.Layout.Profiles, "mid-parked"))
		cmd := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(cfg.Layout.Profiles, "mid"), outside)
		_ = cmd.Run()
	}
	if err := svc.DeleteInstanceAndProfile(in.ID); err == nil {
		t.Fatal("purge under post-verify junction swap succeeded, want refusal")
	}
	if _, err := svc.GetInstance(in.ID); err != nil {
		t.Errorf("record lost after refused purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof", "Preferences")); err != nil {
		t.Errorf("outside directory was harmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.Profiles, "mid-parked", "prof", "Preferences")); err != nil {
		t.Errorf("real profile was moved or harmed: %v", err)
	}
	_ = os.Remove(filepath.Join(cfg.Layout.Profiles, "mid")) // junction link only
}
