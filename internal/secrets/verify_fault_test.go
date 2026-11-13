package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ACL verification fault-injection tests (Node B review fix).
//
// Production persistence verifies the restrictions of EVERY
// credential-bearing file after restricting it — temp file,
// backup temp file, and the final main and backup files — and
// fails closed on any verification failure. These tests substitute
// the verifyRestrictions seam (the production verifier is never
// weakened; it is simply made to report failure for chosen paths)
// and assert the end state on disk, not just error returns.

func failVerifyFor(t *testing.T, match func(path string) bool) {
	t.Helper()
	orig := verifyRestrictions
	verifyRestrictions = func(path string) error {
		if match(path) {
			return errors.New("injected verification failure")
		}
		return orig(path)
	}
	t.Cleanup(func() { verifyRestrictions = orig })
}

func baseName(p string) string { return filepath.Base(p) }

func TestVerifyFailureOnTempAbortsPut(t *testing.T) {
	s, path := newTestStore(t)
	good, _ := s.Put([]byte("good"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failVerifyFor(t, func(p string) bool {
		b := baseName(p)
		return strings.Contains(b, ".tmp-") && !strings.Contains(b, ".bak.tmp-")
	})
	if _, err := s.Put([]byte("bad")); err == nil {
		t.Fatal("put succeeded despite temp-file verification failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("vault file changed despite aborted put")
	}
	if got, err := s.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("prior credential damaged: %q, %v", got, err)
	}
}

func TestVerifyFailureOnBackupTempAbortsPut(t *testing.T) {
	s, path := newTestStore(t)
	good, _ := s.Put([]byte("good"))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	failVerifyFor(t, func(p string) bool {
		return strings.Contains(baseName(p), ".bak.tmp-")
	})
	if _, err := s.Put([]byte("bad")); err == nil {
		t.Fatal("put succeeded despite backup-temp verification failure")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("vault file changed despite aborted put")
	}
	if got, err := s.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("prior credential damaged: %q, %v", got, err)
	}
}

func TestVerifyFailureOnFinalFileFailsPut(t *testing.T) {
	s, path := newTestStore(t)
	good, _ := s.Put([]byte("good"))
	// Fail verification ONLY for the final main file (post-replace
	// stage): every earlier stage succeeds, the write physically
	// lands, yet Put must report failure — never success.
	failVerifyFor(t, func(p string) bool { return p == path })
	if _, err := s.Put([]byte("unreported")); err == nil {
		t.Fatal("put reported success despite final-file verification failure")
	}
	// The prior credential survives on disk: with the injection
	// cleared, a restart loads it (the never-reported new entry
	// may exist as an orphan for GC; the prior data must be whole).
	verifyRestrictions = VerifyFileRestrictions
	s2, rec := reopen(t, path)
	if rec.Unrecoverable {
		t.Fatalf("vault unrecoverable after failed put: %+v", rec)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("prior credential lost: %q, %v", got, err)
	}
}

func TestVerifyFailureAtOpenFailsClosed(t *testing.T) {
	s, path := newTestStore(t)
	ref, _ := s.Put([]byte("one"))
	if _, err := s.Put([]byte("two")); err != nil { // ensures .bak exists
		t.Fatal(err)
	}
	// Backup verification failure at open: fail closed.
	failVerifyFor(t, func(p string) bool { return strings.HasSuffix(p, ".bak") })
	if _, _, err := openWithProtector(path, fakeProtector{}); err == nil {
		t.Fatal("open succeeded despite backup verification failure")
	}
	// Main-file verification failure at open: fail closed.
	failVerifyFor(t, func(p string) bool { return p == path })
	if _, _, err := openWithProtector(path, fakeProtector{}); err == nil {
		t.Fatal("open succeeded despite main-file verification failure")
	}
	// With the injection cleared, the same files open cleanly and
	// the credentials are intact — the failures above never
	// damaged the stored data.
	verifyRestrictions = VerifyFileRestrictions
	s2, rec := reopen(t, path)
	if rec.Unrecoverable || rec.RecoveredFromBackup {
		t.Fatalf("unexpected recovery after clean open: %+v", rec)
	}
	if got, err := s2.Get(ref); err != nil || string(got) != "one" {
		t.Fatalf("credential damaged: %q, %v", got, err)
	}
}
