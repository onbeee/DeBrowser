//go:build windows

package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Windows-only tests: real DPAPI, the real ACL implementation and
// MoveFileEx-based replacement. They run in GitHub Actions on the
// Windows runner (this package's Linux runs use the AES-GCM
// development protector and cannot exercise these paths).

func TestDPAPIRoundTripRealVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.vault")
	s, rec, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if rec.Unrecoverable {
		t.Fatalf("fresh vault unrecoverable: %+v", rec)
	}
	ref, err := s.Put([]byte("proxy-password-123"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// The file on disk must not contain the plaintext anywhere.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "proxy-password-123") {
		t.Fatal("plaintext credential found in vault file")
	}
	s2, _, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.Get(ref)
	if err != nil || string(got) != "proxy-password-123" {
		t.Fatalf("get = %q, %v", got, err)
	}
}

func TestDPAPIEntropyMismatchCannotDecrypt(t *testing.T) {
	// A blob protected under different optional entropy must not
	// decrypt — the same failure class as a vault moved to another
	// Windows user or machine, which the store reports as
	// ErrCorrupt rather than returning wrong data.
	def, err := platformProtector("")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := def.Protect([]byte("topsecret"))
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	other := dpapiProtector{entropy: []byte("DeBrowser/secrets/other")}
	if _, err := other.Unprotect(blob); err == nil {
		t.Fatal("blob decrypted under wrong entropy")
	}
	if plain, err := def.Unprotect(blob); err != nil || string(plain) != "topsecret" {
		t.Fatalf("correct-entropy decrypt = %q, %v", plain, err)
	}
}

func TestACLRestrictAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acl-probe.bin")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestrictFile(path); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("verify after restrict: %v", err)
	}
	// Restricting twice must be a stable no-op success.
	if err := RestrictFile(path); err != nil {
		t.Fatalf("second restrict: %v", err)
	}
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("verify after second restrict: %v", err)
	}
}

func TestReplaceFileOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.bin")
	for i, want := range []string{"one", "two-two", "three-three-three"} {
		src := filepath.Join(dir, "src.bin")
		if err := os.WriteFile(src, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceFile(src, dst); err != nil {
			t.Fatalf("replace %d: %v", i, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != want {
			t.Fatalf("after replace %d: %q, %v", i, got, err)
		}
	}
}

func TestVaultPutFailsInMissingDirectory(t *testing.T) {
	// Opening a not-yet-created vault is fine, but persisting
	// into a nonexistent directory must fail loudly at Put time —
	// never report a credential as stored when it is not.
	bad := filepath.Join(t.TempDir(), "no-such-dir", "secrets.vault")
	s, _, err := Open(bad)
	if err != nil {
		t.Fatalf("open fresh vault: %v", err)
	}
	if _, err := s.Put([]byte("x")); err == nil {
		t.Fatal("put into missing directory succeeded")
	} else if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("unexpected error class: %v", err)
	}
}
