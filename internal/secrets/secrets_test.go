package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeProtector is a reversible stand-in for DPAPI/GCM in the
// platform-neutral logic tests: blobs carry a magic prefix, and
// Unprotect rejects anything without it, which lets tests simulate
// undecryptable entries by damaging stored data.
type fakeProtector struct{}

var fakeMagic = []byte("FAKE1:")

func (fakeProtector) Protect(plain []byte) ([]byte, error) {
	out := append([]byte{}, fakeMagic...)
	return append(out, plain...), nil
}

func (fakeProtector) Unprotect(blob []byte) ([]byte, error) {
	if !bytes.HasPrefix(blob, fakeMagic) {
		return nil, errors.New("fake: undecryptable blob")
	}
	return blob[len(fakeMagic):], nil
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.vault")
	s, rec, err := openWithProtector(path, fakeProtector{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if rec.Unrecoverable || rec.RecoveredFromBackup {
		t.Fatalf("fresh store reported recovery: %+v", rec)
	}
	return s, path
}

func reopen(t *testing.T, path string) (*Store, Recovery) {
	t.Helper()
	s, rec, err := openWithProtector(path, fakeProtector{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return s, rec
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	s, path := newTestStore(t)
	ref, err := s.Put([]byte("s3cret-password"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref == "" {
		t.Fatal("empty ref")
	}
	got, err := s.Get(ref)
	if err != nil || string(got) != "s3cret-password" {
		t.Fatalf("get = %q, %v", got, err)
	}
	// Survives a restart (new Store over the same file).
	s2, _ := reopen(t, path)
	got, err = s2.Get(ref)
	if err != nil || string(got) != "s3cret-password" {
		t.Fatalf("get after reopen = %q, %v", got, err)
	}
	if err := s2.Delete(ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s2.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
	// Delete is idempotent: compensation and cleanup paths may
	// repeat it freely.
	if err := s2.Delete(ref); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := s2.Delete("never-existed"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestConcurrentPuts(t *testing.T) {
	s, path := newTestStore(t)
	const n = 32
	refs := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ref, err := s.Put([]byte{byte(i), byte(i + 1)})
			if err != nil {
				t.Errorf("put %d: %v", i, err)
				return
			}
			refs[i] = ref
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, ref := range refs {
		if ref == "" || seen[ref] {
			t.Fatalf("ref %d missing or duplicated: %q", i, ref)
		}
		seen[ref] = true
		got, err := s.Get(ref)
		if err != nil || len(got) != 2 || int(got[0]) != i {
			t.Fatalf("get %d = %v, %v", i, got, err)
		}
	}
	s2, _ := reopen(t, path)
	if got := len(s2.Refs()); got != n {
		t.Fatalf("reopened vault holds %d entries, want %d", got, n)
	}
}

func TestPutPersistFailureLeavesDiskUnchanged(t *testing.T) {
	s, path := newTestStore(t)
	good, err := s.Put([]byte("good"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	HookBeforePersist = func() error { return errors.New("injected persist failure") }
	defer func() { HookBeforePersist = nil }()
	if _, err := s.Put([]byte("bad")); err == nil {
		t.Fatal("put with failing persist succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("vault file changed despite failed persist")
	}
	// In-memory state is unchanged too: prior data intact, and a
	// reopened store agrees with the disk.
	if got, err := s.Get(good); err != nil || string(got) != "good" {
		t.Fatalf("get good = %q, %v", got, err)
	}
	s2, _ := reopen(t, path)
	if n := len(s2.Refs()); n != 1 {
		t.Fatalf("reopened vault holds %d entries, want 1", n)
	}
}

// The remaining tests verify END STATE consistency between a
// simulated database (a set of live references) and the vault file
// on disk — not merely API return values — following the package's
// consistency protocol exactly as the future ProxyService will.

func vaultRefSet(t *testing.T, path string) map[string]bool {
	t.Helper()
	s, _ := reopen(t, path)
	out := map[string]bool{}
	for _, r := range s.Refs() {
		out[r.Ref] = true
	}
	return out
}

func TestConsistencyCreateWithDBFailureCompensates(t *testing.T) {
	s, path := newTestStore(t)
	ref, err := s.Put([]byte("pw")) // step 1: durable Put
	if err != nil {
		t.Fatal(err)
	}
	// step 2: the database transaction FAILS -> compensate.
	if err := s.Delete(ref); err != nil {
		t.Fatalf("compensating delete: %v", err)
	}
	if got := vaultRefSet(t, path); len(got) != 0 {
		t.Fatalf("vault not empty after compensation: %v", got)
	}
}

func TestConsistencyUpdateSwapAndCleanupFailure(t *testing.T) {
	s, path := newTestStore(t)
	oldRef, _ := s.Put([]byte("old-pw"))
	s.MarkCommitted(oldRef)
	db := map[string]bool{oldRef: true} // simulated live references

	newRef, err := s.Put([]byte("new-pw"))
	if err != nil {
		t.Fatal(err)
	}
	db = map[string]bool{newRef: true} // DB transaction swaps the ref
	s.MarkCommitted(newRef)

	// Cleanup of the old entry FAILS (injected): the old blob must
	// remain as an identifiable orphan, the new state consistent.
	HookBeforePersist = func() error { return errors.New("injected") }
	if err := s.Delete(oldRef); err == nil {
		t.Fatal("delete with failing persist succeeded")
	}
	HookBeforePersist = nil
	disk := vaultRefSet(t, path)
	if !disk[newRef] || !disk[oldRef] {
		t.Fatalf("after failed cleanup, disk = %v, want both refs", disk)
	}
	// Retry succeeds; final disk state equals the DB reference set.
	if err := s.Delete(oldRef); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	disk = vaultRefSet(t, path)
	if len(disk) != len(db) || !disk[newRef] {
		t.Fatalf("final disk = %v, db = %v", disk, db)
	}
	if got, err := s.Get(newRef); err != nil || string(got) != "new-pw" {
		t.Fatalf("get new = %q, %v", got, err)
	}
}

func TestGCSharedReferenceSurvives(t *testing.T) {
	s, path := newTestStore(t)
	shared, _ := s.Put([]byte("shared"))
	s.MarkCommitted(shared)
	// Two database rows reference the same entry; one row is
	// deleted. The live set still contains the ref, so GC must not
	// collect it even when it is old.
	backdate(t, path, shared, 72*time.Hour)
	s2, _ := reopen(t, path)
	removed, err := s2.CollectGarbage(map[string]bool{shared: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("GC removed a live shared reference: %v", removed)
	}
	if _, err := s2.Get(shared); err != nil {
		t.Fatalf("shared entry damaged by GC: %v", err)
	}
}

func TestGCAgeGateAndCollection(t *testing.T) {
	s, path := newTestStore(t)
	young, _ := s.Put([]byte("young-orphan"))
	s.MarkCommitted(young)
	old, _ := s.Put([]byte("old-orphan"))
	s.MarkCommitted(old)
	backdate(t, path, old, 48*time.Hour)
	s2, _ := reopen(t, path)
	removed, err := s2.CollectGarbage(map[string]bool{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Fatalf("removed = %v, want only the old orphan", removed)
	}
	if _, err := s2.Get(young); err != nil {
		t.Fatalf("young orphan wrongly collected: %v", err)
	}
}

func TestGCPendingEntryProtected(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := s.Put([]byte("in-flight")) // not MarkCommitted
	// Age the entry in memory (the file-level equivalent is what
	// the age-gate tests exercise after a reopen): within the
	// process that performed the Put, the pending flag is what
	// protects an entry whose DB commit is still in flight.
	e := s.entries[ref]
	e.CreatedAt = time.Now().Add(-72 * time.Hour)
	s.entries[ref] = e
	removed, err := s.CollectGarbage(map[string]bool{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("GC removed a pending entry: %v", removed)
	}
	s.MarkCommitted(ref)
	removed, err = s.CollectGarbage(map[string]bool{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != ref {
		t.Fatalf("after commit, removed = %v, want the entry", removed)
	}
}

// backdate rewrites one entry's created_at in the vault file, so
// GC age tests do not depend on wall-clock waiting. The store must
// be reopened afterwards (its in-memory copy is stale by design).
func backdate(t *testing.T, path, ref string, age time.Duration) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	e, ok := vf.Entries[ref]
	if !ok {
		t.Fatalf("ref %s not in vault file", ref)
	}
	e.CreatedAt = time.Now().Add(-age)
	vf.Entries[ref] = e
	out, err := json.Marshal(vf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
