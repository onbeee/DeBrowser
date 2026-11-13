package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Recovery error-classification tests (Node B classification
// fix). Open must distinguish three classes of read failure:
// missing (recovery/fresh start allowed), corrupt content —
// invalid JSON or unsupported version (recovery and quarantine
// allowed) — and access failures (permission denied, I/O errors:
// Open fails closed, NOTHING is quarantined, restored over or
// started empty, because the data may be perfectly intact).

// failReadFor substitutes the readVaultFn seam: reads of paths
// matched by fail return err; all other reads delegate to the
// real implementation. Returns a restore function.
func failReadFor(t *testing.T, fail func(path string) bool, err error) func() {
	t.Helper()
	real := readVaultFile
	orig := readVaultFn
	readVaultFn = func(p string) (map[string]entry, error) {
		if fail(p) {
			return nil, err
		}
		return real(p)
	}
	restore := func() { readVaultFn = orig }
	t.Cleanup(restore)
	return restore
}

func classifiedStore(t *testing.T) (string, string, []byte, []byte) {
	t.Helper()
	_, path, good := backupTestStore(t) // main {good,second}, bak {good}
	return path, good, readMain(t, path), readMain(t, path+".bak")
}

func assertNoQuarantine(t *testing.T, vaultPath string) {
	t.Helper()
	matches, err := filepath.Glob(vaultPath + ".corrupt-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("unexpected quarantine files: %v (glob err %v)", matches, err)
	}
	matches, err = filepath.Glob(vaultPath + ".bak.corrupt-*")
	if err != nil || len(matches) != 0 {
		t.Fatalf("unexpected backup quarantine files: %v (glob err %v)", matches, err)
	}
}

// assertAccessFailure pins the fail-closed contract for
// permission/I/O cases: diagnosable error chain, both files
// byte-identical, no quarantine, and a clean open once the
// fault is cleared.
func assertAccessFailure(t *testing.T, path, ref string, mainBefore, bakBefore []byte, openErr, wantErr error) {
	t.Helper()
	if openErr == nil {
		t.Fatal("open succeeded despite an access failure")
	}
	if !errors.Is(openErr, wantErr) {
		t.Fatalf("error not diagnosable: %v (want chain containing %v)", openErr, wantErr)
	}
	if after := readMain(t, path); !bytes.Equal(mainBefore, after) {
		t.Fatal("main vault bytes changed after an access failure")
	}
	if bakBefore != nil {
		if after := readMain(t, path+".bak"); !bytes.Equal(bakBefore, after) {
			t.Fatal("backup bytes changed after an access failure")
		}
	}
	assertNoQuarantine(t, path)
}

// Case 1: main permission-denied, backup valid.
func TestOpenMainPermissionDeniedBackupValid(t *testing.T) {
	path, good, mainBefore, bakBefore := classifiedStore(t)
	clear := failReadFor(t, func(p string) bool { return p == path }, fs.ErrPermission)
	_, _, err := openWithProtector(path, fakeProtector{})
	assertAccessFailure(t, path, good, mainBefore, bakBefore, err, fs.ErrPermission)
	clear()
	s2, rec := reopen(t, path)
	if rec.Unrecoverable || rec.RecoveredFromBackup {
		t.Fatalf("unexpected recovery after fault cleared: %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after fault cleared: %q, %v", got, err)
	}
}

// Case 2: main I/O error, backup valid.
func TestOpenMainIOErrorBackupValid(t *testing.T) {
	path, good, mainBefore, bakBefore := classifiedStore(t)
	clear := failReadFor(t, func(p string) bool { return p == path }, io.ErrUnexpectedEOF)
	_, _, err := openWithProtector(path, fakeProtector{})
	assertAccessFailure(t, path, good, mainBefore, bakBefore, err, io.ErrUnexpectedEOF)
	clear()
	s2, _ := reopen(t, path)
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after fault cleared: %q, %v", got, err)
	}
}

// Case 3: main permission-denied, backup absent — no empty
// vault may be started over the unreadable main file.
func TestOpenMainPermissionDeniedNoBackup(t *testing.T) {
	s, path := newTestStore(t)
	good, err := s.Put([]byte("good")) // single write: no .bak exists
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("test premise broken: unexpected .bak (%v)", err)
	}
	mainBefore := readMain(t, path)
	clear := failReadFor(t, func(p string) bool { return p == path }, fs.ErrPermission)
	_, _, oerr := openWithProtector(path, fakeProtector{})
	assertAccessFailure(t, path, good, mainBefore, nil, oerr, fs.ErrPermission)
	clear()
	s2, _ := reopen(t, path)
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after fault cleared: %q, %v", got, err)
	}
}

// Case 4: main valid, backup permission-denied.
func TestOpenBackupPermissionDeniedMainValid(t *testing.T) {
	path, good, mainBefore, bakBefore := classifiedStore(t)
	clear := failReadFor(t, func(p string) bool { return p == path+".bak" }, fs.ErrPermission)
	_, _, err := openWithProtector(path, fakeProtector{})
	assertAccessFailure(t, path, good, mainBefore, bakBefore, err, fs.ErrPermission)
	clear()
	s2, _ := reopen(t, path)
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after fault cleared: %q, %v", got, err)
	}
}

// Case 5: main genuinely corrupt (invalid JSON), backup valid —
// recovery still works.
func TestOpenCorruptMainRecoversFromBackup(t *testing.T) {
	path, good, _, _ := classifiedStore(t)
	if err := os.WriteFile(path, []byte("{ not a vault"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup || rec.Unrecoverable {
		t.Fatalf("expected clean backup recovery, got %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after recovery: %q, %v", got, err)
	}
}

// Case 6: main version unsupported, backup valid — recovery
// still works.
func TestOpenUnsupportedVersionMainRecoversFromBackup(t *testing.T) {
	path, good, mainBefore, _ := classifiedStore(t)
	var vf vaultFile
	if err := json.Unmarshal(mainBefore, &vf); err != nil {
		t.Fatal(err)
	}
	vf.Version = 99
	bad, err := json.Marshal(vf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup || rec.Unrecoverable {
		t.Fatalf("expected clean backup recovery, got %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after recovery: %q, %v", got, err)
	}
}

// Case 7: main and backup both genuinely corrupt — the
// double-corrupt quarantine mechanism is preserved, original
// bytes retained under the quarantine names.
func TestOpenBothCorruptQuarantined(t *testing.T) {
	path, good, _, _ := classifiedStore(t)
	mainGarbage := []byte("{ main garbage")
	bakGarbage := []byte("{ bak garbage")
	if err := os.WriteFile(path, mainGarbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", bakGarbage, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.Unrecoverable || len(rec.Quarantined) != 2 {
		t.Fatalf("expected double-corrupt quarantine, got %+v", rec)
	}
	contents := map[string]bool{}
	for _, q := range rec.Quarantined {
		raw, err := os.ReadFile(q)
		if err != nil {
			t.Fatal(err)
		}
		contents[string(raw)] = true
	}
	if !contents[string(mainGarbage)] || !contents[string(bakGarbage)] {
		t.Fatalf("quarantined bytes do not match the corrupt originals: %v", contents)
	}
	if _, err := s2.Get(good); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on the empty recovered vault, got %v", err)
	}
}

// Case 8: main missing, backup valid — recovery still works.
func TestOpenMissingMainRecoversFromBackup(t *testing.T) {
	path, good, _, _ := classifiedStore(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup || rec.Unrecoverable {
		t.Fatalf("expected clean backup recovery, got %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after recovery: %q, %v", got, err)
	}
}
