package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Backup-snapshot fault-injection tests (Node B final fix).
//
// persistLocked must FAIL CLOSED when the pre-replace backup of
// the current vault cannot be completed: an existing main vault
// that cannot be read or parsed, or any failure while making the
// snapshot, aborts the persistence with the old main file
// byte-identical and the in-memory state untouched. Only a
// genuinely absent main file (first creation) may skip the
// snapshot. The ten cases below map to the review list; every
// case also asserts that a clean reopen still reads the original
// credentials afterwards.

func backupTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	s, path := newTestStore(t)
	good, err := s.Put([]byte("good"))
	if err != nil {
		t.Fatal(err)
	}
	// A second write so that .bak holds {good} and the main file
	// holds {good, second}: both files exist and are healthy.
	if _, err := s.Put([]byte("second")); err != nil {
		t.Fatal(err)
	}
	return s, path, good
}

func assertAborted(t *testing.T, s *Store, path, ref string, before []byte) {
	t.Helper()
	if _, err := s.Put([]byte("must-not-land")); err == nil {
		t.Fatal("put reported success despite backup failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("main vault bytes changed despite aborted persistence")
	}
	if got, err := s.Get(ref); err != nil || string(got) != "good" {
		t.Fatalf("in-memory credential damaged: %q, %v", got, err)
	}
}

func assertCleanReopen(t *testing.T, path, ref string) {
	t.Helper()
	s2, rec := reopen(t, path)
	if rec.Unrecoverable || rec.RecoveredFromBackup {
		t.Fatalf("unexpected recovery on clean reopen: %+v", rec)
	}
	if got, err := s2.Get(ref); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after reopen: %q, %v", got, err)
	}
}

func readMain(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Case 1: main vault exists but reading it returns a permission
// error.
func TestBackupAbortOnMainReadPermissionError(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	orig := readVaultFn
	readVaultFn = func(string) (map[string]entry, error) { return nil, fs.ErrPermission }
	defer func() { readVaultFn = orig }()

	_, err := s.Put([]byte("must-not-land"))
	if err == nil {
		t.Fatal("put reported success despite backup failure")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("failure cause not diagnosable (want fs.ErrPermission chain): %v", err)
	}
	after := readMain(t, path)
	if !bytes.Equal(before, after) {
		t.Fatal("main vault bytes changed despite aborted persistence")
	}
	if got, err := s.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("in-memory credential damaged: %q, %v", got, err)
	}
	readVaultFn = orig
	assertCleanReopen(t, path, good)
}

// Case 2: main vault exists but reading it returns an I/O error.
func TestBackupAbortOnMainReadIOError(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	orig := readVaultFn
	ioErr := io.ErrUnexpectedEOF
	readVaultFn = func(string) (map[string]entry, error) { return nil, ioErr }
	defer func() { readVaultFn = orig }()

	_, err := s.Put([]byte("must-not-land"))
	if err == nil {
		t.Fatal("put reported success despite backup failure")
	}
	if !errors.Is(err, ioErr) {
		t.Fatalf("failure cause not diagnosable: %v", err)
	}
	if after := readMain(t, path); !bytes.Equal(before, after) {
		t.Fatal("main vault bytes changed despite aborted persistence")
	}
	readVaultFn = orig
	assertCleanReopen(t, path, good)
}

// Case 3: main vault JSON is corrupt on disk.
func TestBackupAbortOnCorruptMainJSON(t *testing.T) {
	s, path, good := backupTestStore(t)
	garbage := []byte("{ this is not a vault")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put([]byte("must-not-land")); err == nil {
		t.Fatal("put overwrote a corrupt main vault")
	}
	if after := readMain(t, path); !bytes.Equal(garbage, after) {
		t.Fatal("corrupt main vault was silently overwritten")
	}
	// The explicit recovery protocol at Open handles the corrupt
	// main: restore from .bak and read the original credential.
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup {
		t.Fatalf("expected backup recovery, got %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after recovery: %q, %v", got, err)
	}
}

// Case 4: main vault carries an unsupported format version.
func TestBackupAbortOnUnsupportedMainVersion(t *testing.T) {
	s, path, good := backupTestStore(t)
	raw := readMain(t, path)
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
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
	if _, err := s.Put([]byte("must-not-land")); err == nil {
		t.Fatal("put overwrote an unsupported-version main vault")
	}
	if after := readMain(t, path); !bytes.Equal(bad, after) {
		t.Fatal("unsupported-version main vault was silently overwritten")
	}
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup {
		t.Fatalf("expected backup recovery, got %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("credential unreadable after recovery: %q, %v", got, err)
	}
}

// Case 5: reading the raw bytes of the (valid) main vault fails.
func TestBackupAbortOnRawReadFailure(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	orig := readFileFn
	readFileFn = func(p string) ([]byte, error) {
		if p == path {
			return nil, errors.New("simulated raw read failure")
		}
		return orig(p)
	}
	defer func() { readFileFn = orig }()
	assertAborted(t, s, path, good, before)
	readFileFn = orig
	assertCleanReopen(t, path, good)
}

// Case 6: creating the backup temp file fails.
func TestBackupAbortOnBackupTempCreateFailure(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	orig := writeTempFn
	writeTempFn = func(dir, prefix string, raw []byte) (string, error) {
		if strings.Contains(prefix, ".bak.tmp-") {
			return "", errors.New("simulated backup temp create failure")
		}
		return orig(dir, prefix, raw)
	}
	defer func() { writeTempFn = orig }()
	assertAborted(t, s, path, good, before)
	writeTempFn = orig
	assertCleanReopen(t, path, good)
}

// Case 7: writing the backup temp file fails midway.
func TestBackupAbortOnBackupTempWriteFailure(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	orig := writeTempFn
	writeTempFn = func(dir, prefix string, raw []byte) (string, error) {
		if strings.Contains(prefix, ".bak.tmp-") {
			// Simulate a write that starts and then fails: a
			// partial temp file exists and an error is returned.
			f, err := os.CreateTemp(dir, prefix+"*")
			if err != nil {
				return "", err
			}
			_, _ = f.Write(raw[:len(raw)/2])
			_ = f.Close()
			return f.Name(), errors.New("simulated backup write failure")
		}
		return orig(dir, prefix, raw)
	}
	defer func() { writeTempFn = orig }()
	assertAborted(t, s, path, good, before)
	writeTempFn = orig
	assertCleanReopen(t, path, good)
}

// Case 8: ACL verification of the backup temp file fails.
func TestBackupAbortOnBackupTempVerifyFailure(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	failVerifyFor(t, func(p string) bool {
		return strings.Contains(filepath.Base(p), ".bak.tmp-")
	})
	assertAborted(t, s, path, good, before)
	verifyRestrictions = VerifyFileRestrictions
	assertCleanReopen(t, path, good)
}

// Case 9: the atomic replace of the backup file fails.
func TestBackupAbortOnBackupReplaceFailure(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	orig := replaceFileFn
	replaceFileFn = func(src, dst string) error {
		if strings.HasSuffix(dst, ".bak") {
			return errors.New("simulated backup replace failure")
		}
		return orig(src, dst)
	}
	defer func() { replaceFileFn = orig }()
	assertAborted(t, s, path, good, before)
	replaceFileFn = orig
	assertCleanReopen(t, path, good)
}

// Case 10: ACL verification of the FINAL backup file fails — the
// abort must happen before the main file is replaced.
func TestBackupAbortOnFinalBackupVerifyFailure(t *testing.T) {
	s, path, good := backupTestStore(t)
	before := readMain(t, path)
	failVerifyFor(t, func(p string) bool { return p == path+".bak" })
	assertAborted(t, s, path, good, before)
	verifyRestrictions = VerifyFileRestrictions
	assertCleanReopen(t, path, good)
}
