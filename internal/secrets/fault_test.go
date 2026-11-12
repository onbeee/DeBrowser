package secrets

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fault-injection and recovery tests: corrupted files, torn
// writes, backup recovery and per-entry isolation. Every test
// asserts the FINAL on-disk state a fresh Open observes — the same
// state the application would see after a crash and restart.

func TestCorruptSingleEntryIsolated(t *testing.T) {
	s, path := newTestStore(t)
	good, _ := s.Put([]byte("good"))
	victim, _ := s.Put([]byte("victim"))

	// Damage exactly one entry's ciphertext in the file (valid
	// base64, undecryptable content).
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	e := vf.Entries[victim]
	e.Data = base64.StdEncoding.EncodeToString([]byte("garbage-not-a-blob"))
	vf.Entries[victim] = e
	out, _ := json.Marshal(vf)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	s2, rec := reopen(t, path)
	if rec.Unrecoverable || rec.RecoveredFromBackup {
		t.Fatalf("single-entry damage triggered vault-level recovery: %+v", rec)
	}
	if _, err := s2.Get(victim); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("victim get err = %v, want ErrCorrupt", err)
	}
	if got, err := s2.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("good entry affected: %q, %v", got, err)
	}
	// The corrupt entry is retained and reported, not dropped.
	found := false
	for _, r := range s2.Refs() {
		if r.Ref == victim && r.Corrupt {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt entry not reported via Refs()")
	}
	// The store stays fully usable.
	if _, err := s2.Put([]byte("after")); err != nil {
		t.Fatalf("put after corruption: %v", err)
	}
}

func TestMainCorruptRecoversFromBackup(t *testing.T) {
	s, path := newTestStore(t)
	refA, _ := s.Put([]byte("A")) // main = {A}, no backup yet
	refB, _ := s.Put([]byte("B")) // backup = {A}, main = {A,B}

	if err := os.WriteFile(path, []byte("{torn-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup || rec.Unrecoverable {
		t.Fatalf("recovery = %+v, want RecoveredFromBackup only", rec)
	}
	if got, err := s2.Get(refA); err != nil || string(got) != "A" {
		t.Fatalf("A after recovery = %q, %v", got, err)
	}
	// B was only in the lost main file: its reference is now
	// dangling and must surface as ErrNotFound (the service layer
	// flags the owning row "credential unavailable").
	if _, err := s2.Get(refB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B get err = %v, want ErrNotFound", err)
	}
	// The restored backup became the main file: a further reopen
	// is clean and still holds A.
	s3, rec3 := reopen(t, path)
	if rec3.RecoveredFromBackup {
		t.Fatal("second reopen recovered again; restore did not stick")
	}
	if _, err := s3.Get(refA); err != nil {
		t.Fatalf("A lost after restore: %v", err)
	}
}

func TestBothCorruptQuarantinedNotDeleted(t *testing.T) {
	s, path := newTestStore(t)
	if _, err := s.Put([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put([]byte("B")); err != nil { // ensure .bak exists
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("junk-main"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".bak", []byte("junk-bak"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.Unrecoverable {
		t.Fatalf("recovery = %+v, want Unrecoverable", rec)
	}
	if len(rec.Quarantined) != 2 {
		t.Fatalf("quarantined = %v, want both files", rec.Quarantined)
	}
	for _, q := range rec.Quarantined {
		data, err := os.ReadFile(q)
		if err != nil || !strings.HasPrefix(string(data), "junk-") {
			t.Fatalf("quarantined file %s lost its content: %v", q, err)
		}
	}
	// The store starts empty and remains fully usable.
	if n := len(s2.Refs()); n != 0 {
		t.Fatalf("store holds %d entries after quarantine, want 0", n)
	}
	ref, err := s2.Put([]byte("fresh"))
	if err != nil {
		t.Fatalf("put after quarantine: %v", err)
	}
	if got, err := s2.Get(ref); err != nil || string(got) != "fresh" {
		t.Fatalf("get fresh = %q, %v", got, err)
	}
}

func TestTornTempFilesIgnoredAndCleaned(t *testing.T) {
	s, path := newTestStore(t)
	ref, _ := s.Put([]byte("stable"))
	dir := filepath.Dir(path)
	for _, name := range []string{"secrets.vault.tmp-123456", "secrets.vault.bak.tmp-789"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("torn"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, rec := reopen(t, path)
	if rec.Unrecoverable || rec.RecoveredFromBackup {
		t.Fatalf("torn temps triggered recovery: %+v", rec)
	}
	if got, err := s2.Get(ref); err != nil || string(got) != "stable" {
		t.Fatalf("get = %q, %v", got, err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "secrets.vault.*tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("stale temp files not cleaned: %v", matches)
	}
}

func TestMissingMainRestoresBackup(t *testing.T) {
	s, path := newTestStore(t)
	refA, _ := s.Put([]byte("A"))
	if _, err := s.Put([]byte("B")); err != nil { // creates .bak = {A}
		t.Fatal(err)
	}
	// Simulate losing the main file entirely (for example a
	// failed external copy): the backup must take over.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s2, rec := reopen(t, path)
	if !rec.RecoveredFromBackup {
		t.Fatalf("recovery = %+v, want RecoveredFromBackup", rec)
	}
	if got, err := s2.Get(refA); err != nil || string(got) != "A" {
		t.Fatalf("A = %q, %v", got, err)
	}
}

func TestVaultFilePermissionsRestricted(t *testing.T) {
	_, path := newTestStore(t)
	s, _ := reopen(t, path)
	if _, err := s.Put([]byte("x")); err != nil {
		t.Fatal(err)
	}
	// Both the vault and its backup must verify as restricted
	// (Windows ACL / Unix 0600, per platform implementation).
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("vault permissions: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		if err := VerifyFileRestrictions(path + ".bak"); err != nil {
			t.Fatalf("backup permissions: %v", err)
		}
	}
}
