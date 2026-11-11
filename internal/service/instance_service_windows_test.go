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
