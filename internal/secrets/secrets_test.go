package secrets

import (
	"bytes"
	"encoding/base64"
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
	deleted, err := s2.DeleteIfUnreferenced(ref, noRefs)
	if err != nil || !deleted {
		t.Fatalf("cleanup delete = %v, %v; want true, nil", deleted, err)
	}
	if _, err := s2.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
	// Cleanup is idempotent: repeating it is a no-op success.
	deleted, err = s2.DeleteIfUnreferenced(ref, noRefs)
	if err != nil || deleted {
		t.Fatalf("second cleanup = %v, %v; want false, nil", deleted, err)
	}
	deleted, err = s2.DeleteIfUnreferenced("never-existed", noRefs)
	if err != nil || deleted {
		t.Fatalf("cleanup of missing ref = %v, %v; want false, nil", deleted, err)
	}
}

// noRefs is a live-reference check that reports nothing referenced,
// for tests whose simulated database is empty.
func noRefs(string) (bool, error) { return false, nil }

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
	// step 2: the database transaction FAILS -> compensate. The
	// ref is still pending (never committed), so compensation is
	// the dedicated, structurally safe path.
	deleted, err := s.DeleteCompensating(ref)
	if err != nil || !deleted {
		t.Fatalf("compensating delete = %v, %v; want true, nil", deleted, err)
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
	stillUsed := func(r string) (bool, error) { return db[r], nil }
	HookBeforePersist = func() error { return errors.New("injected") }
	if _, err := s.DeleteIfUnreferenced(oldRef, stillUsed); err == nil {
		t.Fatal("cleanup with failing persist succeeded")
	}
	HookBeforePersist = nil
	disk := vaultRefSet(t, path)
	if !disk[newRef] || !disk[oldRef] {
		t.Fatalf("after failed cleanup, disk = %v, want both refs", disk)
	}
	// Retry succeeds; final disk state equals the DB reference set.
	deleted, err := s.DeleteIfUnreferenced(oldRef, stillUsed)
	if err != nil || !deleted {
		t.Fatalf("retry cleanup = %v, %v; want true, nil", deleted, err)
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

// ---- Shared-reference safety (Node B review fix) ------------------
//
// Several database rows may reference ONE vault entry. The store
// has no unconditional delete: business cleanup must prove the
// reference is unused, and compensation only ever applies to a
// pending, never-committed entry of this process.

func TestSharedReferenceSurvivesUntilLastRowRemoved(t *testing.T) {
	s, path := newTestStore(t)
	ref, _ := s.Put([]byte("shared-pw"))
	s.MarkCommitted(ref)
	// Simulated database: rows A and B share the reference.
	db := map[string]string{"rowA": ref, "rowB": ref}
	stillUsed := func(r string) (bool, error) {
		for _, v := range db {
			if v == r {
				return true, nil
			}
		}
		return false, nil
	}
	// Delete row A and run cleanup: the credential must survive
	// untouched because row B still uses it.
	delete(db, "rowA")
	deleted, err := s.DeleteIfUnreferenced(ref, stillUsed)
	if err != nil || deleted {
		t.Fatalf("cleanup with a live sharer = %v, %v; want false, nil", deleted, err)
	}
	if got, err := s.Get(ref); err != nil || string(got) != "shared-pw" {
		t.Fatalf("shared credential damaged: %q, %v", got, err)
	}
	// Delete row B: now cleanup may remove the entry.
	delete(db, "rowB")
	deleted, err = s.DeleteIfUnreferenced(ref, stillUsed)
	if err != nil || !deleted {
		t.Fatalf("final cleanup = %v, %v; want true, nil", deleted, err)
	}
	if _, err := s.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after final cleanup err = %v, want ErrNotFound", err)
	}
	if disk := vaultRefSet(t, path); len(disk) != 0 {
		t.Fatalf("disk not empty after final cleanup: %v", disk)
	}
}

func TestDeleteCompensatingRefusesCommittedRef(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := s.Put([]byte("committed-pw"))
	s.MarkCommitted(ref)
	// Compensation for a committed (potentially shared) reference
	// must be refused and must not touch the entry.
	if _, err := s.DeleteCompensating(ref); !errors.Is(err, ErrNotCompensable) {
		t.Fatalf("compensating a committed ref err = %v, want ErrNotCompensable", err)
	}
	if got, err := s.Get(ref); err != nil || string(got) != "committed-pw" {
		t.Fatalf("entry damaged by refused compensation: %q, %v", got, err)
	}
	// Compensation of a nonexistent ref is an idempotent no-op.
	deleted, err := s.DeleteCompensating("never-existed")
	if err != nil || deleted {
		t.Fatalf("compensating missing ref = %v, %v; want false, nil", deleted, err)
	}
}

func TestDeleteIfUnreferencedRequiresWorkingCheck(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := s.Put([]byte("pw"))
	s.MarkCommitted(ref)
	if _, err := s.DeleteIfUnreferenced(ref, nil); err == nil {
		t.Fatal("nil live-reference check accepted")
	}
	broken := func(string) (bool, error) { return false, errors.New("db unavailable") }
	if _, err := s.DeleteIfUnreferenced(ref, broken); err == nil {
		t.Fatal("failing live-reference check accepted")
	}
	if got, err := s.Get(ref); err != nil || string(got) != "pw" {
		t.Fatalf("entry damaged by rejected cleanups: %q, %v", got, err)
	}
}

func TestConcurrentSharedRefCleanup(t *testing.T) {
	s, _ := newTestStore(t)
	ref, _ := s.Put([]byte("shared"))
	s.MarkCommitted(ref)
	const rows = 16
	// The documented caller contract, modeled faithfully: one
	// service mutex serializes DB reference changes AND cleanup
	// calls, so check-and-delete is atomic w.r.t. sharing.
	var mu sync.Mutex
	db := map[string]string{}
	for i := 0; i < rows; i++ {
		db[string(rune('a'+i))] = ref
	}
	stillUsed := func(r string) (bool, error) {
		for _, v := range db {
			if v == r {
				return true, nil
			}
		}
		return false, nil
	}
	ids := make([]string, 0, len(db))
	for id := range db {
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			delete(db, id)
			if _, err := s.DeleteIfUnreferenced(ref, stillUsed); err != nil {
				t.Errorf("cleanup: %v", err)
				return
			}
			// Invariant at every step: while any row references
			// the entry, the entry must exist (no dangling ref).
			if len(db) > 0 && !s.Has(ref) {
				t.Errorf("entry deleted while %d rows still reference it", len(db))
			}
		}(id)
	}
	wg.Wait()
	if s.Has(ref) {
		t.Fatal("entry survived after the last referencing row was removed")
	}
	if _, err := s.Get(ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("final get err = %v, want ErrNotFound", err)
	}
}

// ---- Post-write content verification ------------------------------

func writeVaultVariant(t *testing.T, mutate func(*vaultFile)) string {
	t.Helper()
	s, path := newTestStore(t)
	if _, err := s.Put([]byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put([]byte("two")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatal(err)
	}
	want := cloneEntries(vf.Entries)
	mutate(&vf)
	out, err := json.Marshal(vf)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "variant.vault")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyWrittenVault(p, want); err == nil {
		t.Fatal("tampered variant passed verification")
	}
	// The untampered original must verify cleanly.
	if err := verifyWrittenVault(path, want); err != nil {
		t.Fatalf("original vault failed verification: %v", err)
	}
	return p
}

func TestVerifyWrittenVaultExactComparison(t *testing.T) {
	t.Run("same count, renamed ref", func(t *testing.T) {
		writeVaultVariant(t, func(vf *vaultFile) {
			for ref, e := range vf.Entries {
				delete(vf.Entries, ref)
				vf.Entries["00000000-0000-4000-8000-000000000000"] = e
				break
			}
		})
	})
	t.Run("same count, altered blob", func(t *testing.T) {
		writeVaultVariant(t, func(vf *vaultFile) {
			for ref, e := range vf.Entries {
				e.Data = base64.StdEncoding.EncodeToString([]byte("FAKE1:different"))
				vf.Entries[ref] = e
				break
			}
		})
	})
	t.Run("altered created_at", func(t *testing.T) {
		writeVaultVariant(t, func(vf *vaultFile) {
			for ref, e := range vf.Entries {
				e.CreatedAt = e.CreatedAt.Add(time.Hour)
				vf.Entries[ref] = e
				break
			}
		})
	})
	t.Run("extra entry", func(t *testing.T) {
		writeVaultVariant(t, func(vf *vaultFile) {
			vf.Entries["11111111-1111-4111-8111-111111111111"] = entry{CreatedAt: time.Now(), Data: "AAAA"}
		})
	})
	t.Run("missing entry", func(t *testing.T) {
		writeVaultVariant(t, func(vf *vaultFile) {
			for ref := range vf.Entries {
				delete(vf.Entries, ref)
				break
			}
		})
	})
	t.Run("wrong version", func(t *testing.T) {
		writeVaultVariant(t, func(vf *vaultFile) { vf.Version = 99 })
	})
}

func TestPersistDetectsPostReplaceTampering(t *testing.T) {
	s, _ := newTestStore(t)
	prior, _ := s.Put([]byte("prior"))
	// Simulate a torn/misdirected write: after the atomic replace,
	// damage the just-written file's new entry before the store's
	// verification read. The persist MUST fail rather than report
	// success, and in-memory state must stay at the prior state.
	HookAfterReplace = func(p string) {
		raw, err := os.ReadFile(p)
		if err != nil {
			return
		}
		var vf vaultFile
		if err := json.Unmarshal(raw, &vf); err != nil {
			return
		}
		for ref, e := range vf.Entries {
			if ref != prior {
				e.Data = base64.StdEncoding.EncodeToString([]byte("FAKE1:tampered"))
				vf.Entries[ref] = e
			}
		}
		out, _ := json.Marshal(vf)
		_ = os.WriteFile(p, out, 0o600)
	}
	defer func() { HookAfterReplace = nil }()
	if _, err := s.Put([]byte("victim")); err == nil {
		t.Fatal("put succeeded despite post-replace tampering")
	}
	HookAfterReplace = nil
	if got, err := s.Get(prior); err != nil || string(got) != "prior" {
		t.Fatalf("prior entry damaged in memory: %q, %v", got, err)
	}
	if n := len(s.Refs()); n != 1 {
		t.Fatalf("in-memory store holds %d entries after failed put, want 1", n)
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
