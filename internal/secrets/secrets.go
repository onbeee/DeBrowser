// Package secrets is DeBrowser's portable credential vault: the one
// place proxy passwords, protocol UUIDs, private keys and
// subscription URLs are allowed to exist at rest.
//
// Storage model (Stage 4 design, revised report §4):
//
//   - All secrets live in ONE file, data/secrets.vault, inside the
//     portable directory. SQLite only ever holds the opaque
//     reference (secret_ref); plaintext never reaches the database,
//     logs, settings files or frontend state.
//   - Each entry is encrypted by a platform Protector — Windows
//     DPAPI (CurrentUser) in production. DPAPI binds decryption to
//     the same Windows user on the same machine: moving the folder
//     on one machine keeps working, moving it to another machine or
//     user makes entries undecryptable, and this package reports
//     that honestly (ErrCorrupt per entry) instead of pretending
//     the credential is portable or silently dropping it.
//   - Blobs are IMMUTABLE. An update writes a brand-new entry under
//     a new reference and the caller swaps the database reference;
//     the old entry is deleted afterwards. There is no in-place
//     mutation, so no crash can tear a credential in half.
//
// Consistency protocol with SQLite (implemented by callers, made
// possible by this API's ordering guarantees):
//
//   - Create: Put (durable) FIRST, then commit the DB row holding
//     the reference. If the DB commit fails, compensate with
//     DeleteCompensating — valid only while the reference is still
//     pending (created by this process, never committed), which
//     makes it structurally incapable of touching a credential any
//     database row uses. Put only reports success after the new
//     vault file is atomically in place, so the database can never
//     point at a secret that was not persisted.
//   - Update: Put the new secret, swap the reference in one DB
//     transaction, then clean up the old reference with
//     DeleteIfUnreferenced. A cleanup failure leaves an
//     identifiable orphan, never a dangling reference.
//   - Delete: remove the DB row first, then clean up with
//     DeleteIfUnreferenced.
//   - Shared references: several DB rows may reference one entry.
//     There is deliberately NO unconditional delete in this API:
//     business cleanup goes through DeleteIfUnreferenced, whose
//     caller-supplied check runs under the vault lock at decision
//     time, and garbage collection takes the complete live set.
//     Callers (the future ProxyService) must serialize their DB
//     mutations that create or drop references together with these
//     cleanup calls under one service-level mutex, so the check
//     and the delete are atomic with respect to reference changes.
//   - Garbage: CollectGarbage removes entries no database row
//     references, but only when they are older than 24 hours and
//     not pending in this process — the age gate covers the crash
//     window between a durable Put and its DB commit.
//
// Durability: every mutation rewrites the vault through
// write-temp → fsync → restrict-permissions → snapshot the previous
// good file to secrets.vault.bak → atomic replace (Windows
// MoveFileEx with REPLACE_EXISTING|WRITE_THROUGH; POSIX rename on
// Unix). Open recovers from the backup when the main file is
// corrupt, and quarantines both files (never deletes them) when
// neither parses.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNotFound reports that a reference has no entry in the vault.
// Callers treat it as "credential unavailable" (for example after a
// backup recovery rolled the vault back past the entry's creation):
// the owning database row survives and must be flagged, never
// silently treated as "no credential configured".
var ErrNotFound = errors.New("secrets: reference not found")

// ErrCorrupt reports that one entry exists but cannot be decrypted
// (wrong Windows user/machine for DPAPI, or damaged ciphertext).
// Corruption is per-entry: every other entry stays usable.
var ErrCorrupt = errors.New("secrets: entry cannot be decrypted")

// Protector encrypts and decrypts single credential blobs. The
// Windows implementation uses DPAPI; see the platform files.
type Protector interface {
	Protect(plain []byte) ([]byte, error)
	Unprotect(blob []byte) ([]byte, error)
}

// RefInfo describes one vault entry without exposing its content.
type RefInfo struct {
	Ref       string
	CreatedAt time.Time
	// Corrupt is true when the entry's blob cannot be decrypted in
	// this environment (for example a vault copied from another
	// Windows user). The entry is retained, not dropped.
	Corrupt bool
}

// Recovery reports what Open had to do to obtain a usable vault.
type Recovery struct {
	// RecoveredFromBackup is true when the main vault file was
	// missing or corrupt and the .bak snapshot was restored.
	RecoveredFromBackup bool
	// Unrecoverable is true when neither the main file nor the
	// backup parsed; both were quarantined (renamed, not deleted)
	// and the store started empty.
	Unrecoverable bool
	// Quarantined lists the renamed corrupt files, for diagnostics.
	Quarantined []string
}

type entry struct {
	CreatedAt time.Time `json:"created_at"`
	Data      string    `json:"data"` // base64 of the Protector blob
}

type vaultFile struct {
	Version int              `json:"version"`
	Entries map[string]entry `json:"entries"`
}

const vaultVersion = 1

// orphanAge is the minimum age before CollectGarbage may remove an
// unreferenced entry (see the package documentation).
const orphanAge = 24 * time.Hour

// HookBeforePersist, when non-nil, runs at the start of every vault
// persistence and may abort it by returning an error. It is a
// deterministic fault-injection seam for tests (production code
// never sets it), in the same style as pfiles.HookBeforeRename.
var HookBeforePersist func() error

// HookAfterReplace, when non-nil, runs inside persistLocked after
// the main vault file has been atomically replaced but before the
// post-write content verification. Test-only fault-injection seam
// (production code never sets it): it lets tests corrupt the
// just-written file and prove the verification catches it.
var HookAfterReplace func(path string)

// verifyRestrictions is the internal call point for
// VerifyFileRestrictions. Tests may substitute it to inject
// permission-verification failures for specific paths (temp file,
// backup, final file) without weakening the production check,
// which is always the platform implementation; production code
// never reassigns it.
var verifyRestrictions = VerifyFileRestrictions

// Store is the credential vault. All methods are safe for
// concurrent use; mutations are serialized internally. The
// single-instance lock guarantees one app process owns the file.
type Store struct {
	mu      sync.Mutex
	path    string
	prot    Protector
	entries map[string]entry
	corrupt map[string]bool
	pending map[string]bool // Put in this process, not yet MarkCommitted
}

// Open loads (or initializes) the vault at path using the platform
// protector, applying the recovery protocol described in the
// package documentation. It also tightens the vault file's
// permissions (Windows ACL / Unix mode) and FAILS if they cannot be
// tightened — a vault that cannot be restricted is not opened.
func Open(path string) (*Store, Recovery, error) {
	prot, err := platformProtector(path)
	if err != nil {
		return nil, Recovery{}, err
	}
	return openWithProtector(path, prot)
}

func openWithProtector(path string, prot Protector) (*Store, Recovery, error) {
	s := &Store{
		path:    path,
		prot:    prot,
		entries: map[string]entry{},
		corrupt: map[string]bool{},
		pending: map[string]bool{},
	}
	var rec Recovery

	cleanStaleTemps(path)

	main, mainErr := readVaultFile(path)
	switch {
	case mainErr == nil:
		s.entries = main
	default:
		// Main file missing or corrupt: try the backup snapshot.
		bakPath := path + ".bak"
		bak, bakErr := readVaultFile(bakPath)
		if bakErr == nil {
			s.entries = bak
			rec.RecoveredFromBackup = true
			// Restore the backup as the main file so the next
			// mutation starts from a healthy base.
			if err := s.persistLocked(s.entries); err != nil {
				return nil, rec, fmt.Errorf("secrets: restore backup vault: %w", err)
			}
		} else if errors.Is(mainErr, os.ErrNotExist) && errors.Is(bakErr, os.ErrNotExist) {
			// Fresh vault: nothing on disk yet. Not an error.
		} else {
			// Both unreadable for a reason other than "never
			// existed": quarantine the corrupt files (rename, never
			// delete) and start empty; every database reference
			// will resolve to ErrNotFound and be flagged.
			for _, p := range []string{path, bakPath} {
				if _, err := os.Stat(p); err == nil {
					q := fmt.Sprintf("%s.corrupt-%d", p, time.Now().Unix())
					if err := os.Rename(p, q); err == nil {
						rec.Quarantined = append(rec.Quarantined, q)
					}
				}
			}
			rec.Unrecoverable = true
		}
	}

	// Permission discipline: an existing vault file must be
	// restricted to the current user (and SYSTEM/Administrators on
	// Windows). Fail closed when that cannot be established.
	if _, err := os.Stat(path); err == nil {
		if err := RestrictFile(path); err != nil {
			return nil, rec, fmt.Errorf("secrets: restrict vault permissions: %w", err)
		}
		if err := verifyRestrictions(path); err != nil {
			return nil, rec, fmt.Errorf("secrets: verify vault permissions: %w", err)
		}
		if _, err := os.Stat(path + ".bak"); err == nil {
			if err := RestrictFile(path + ".bak"); err != nil {
				return nil, rec, fmt.Errorf("secrets: restrict vault backup permissions: %w", err)
			}
			if err := verifyRestrictions(path + ".bak"); err != nil {
				return nil, rec, fmt.Errorf("secrets: verify vault backup permissions: %w", err)
			}
		}
	}

	s.probeCorruptionLocked()
	return s, rec, nil
}

// probeCorruptionLocked tries to decrypt every entry once, marking
// the ones this environment cannot decrypt. Blobs are retained.
func (s *Store) probeCorruptionLocked() {
	for ref, e := range s.entries {
		blob, err := base64.StdEncoding.DecodeString(e.Data)
		if err != nil {
			s.corrupt[ref] = true
			continue
		}
		if _, err := s.prot.Unprotect(blob); err != nil {
			s.corrupt[ref] = true
		}
	}
}

// Put encrypts plain and stores it under a fresh reference. It
// returns success ONLY after the updated vault file has been
// durably and atomically persisted (see package docs) — callers
// may therefore commit a database row referencing the returned ref
// immediately. The ref stays "pending" (GC-protected) until
// MarkCommitted.
func (s *Store) Put(plain []byte) (string, error) {
	blob, err := s.prot.Protect(plain)
	if err != nil {
		return "", fmt.Errorf("secrets: protect: %w", err)
	}
	ref, err := newRef()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneEntries(s.entries)
	next[ref] = entry{CreatedAt: time.Now().UTC(), Data: base64.StdEncoding.EncodeToString(blob)}
	if err := s.persistLocked(next); err != nil {
		return "", err
	}
	s.entries = next
	s.pending[ref] = true
	return ref, nil
}

// Get decrypts the entry for ref. ErrNotFound means the vault has
// no such entry; ErrCorrupt means it exists but cannot be decrypted
// in this environment. Both are "credential unavailable" for
// callers — never an invitation to fall back to no credential.
func (s *Store) Get(ref string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ref]
	if !ok {
		return nil, ErrNotFound
	}
	if s.corrupt[ref] {
		return nil, ErrCorrupt
	}
	blob, err := base64.StdEncoding.DecodeString(e.Data)
	if err != nil {
		s.corrupt[ref] = true
		return nil, ErrCorrupt
	}
	plain, err := s.prot.Unprotect(blob)
	if err != nil {
		s.corrupt[ref] = true
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return plain, nil
}

// Has reports whether ref has an entry (decryptable or not).
func (s *Store) Has(ref string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[ref]
	return ok
}

// ErrNotCompensable reports a DeleteCompensating call for a
// reference that is not a pending, never-committed entry of this
// process. The entry is left untouched.
var ErrNotCompensable = errors.New("secrets: reference is not compensable")

// DeleteCompensating removes the entry created by a Put in THIS
// process whose database commit then failed. It is valid ONLY
// while ref is still pending (Put but never MarkCommitted): a
// pending reference was never published to the database, so no row
// can reference it and no other row can share it — compensation
// therefore cannot damage a live credential. Calling it for a
// committed or unknown-process reference fails with
// ErrNotCompensable and deletes nothing. Deleting a missing ref
// returns (false, nil): compensation is idempotent.
func (s *Store) DeleteCompensating(ref string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[ref]; !ok {
		return false, nil
	}
	if !s.pending[ref] {
		return false, ErrNotCompensable
	}
	return s.deleteLocked(ref)
}

// DeleteIfUnreferenced removes ref's entry ONLY if the
// caller-supplied stillReferenced check reports that no database
// row references it anymore. The check runs under the vault lock,
// at decision time, so the answer and the deletion are one atomic
// step from the vault's perspective; callers must additionally
// serialize reference-creating DB writes against this call (see
// the package documentation's caller contract). A nil check is
// rejected: business cleanup must never rest on an unverified
// assumption. When the entry is kept, the result is (false, nil).
// Deleting a missing ref returns (false, nil) — cleanup is
// idempotent and may be retried freely. A persistence failure
// leaves the entry in place (an identifiable orphan) and returns
// the error.
func (s *Store) DeleteIfUnreferenced(ref string, stillReferenced func(ref string) (bool, error)) (bool, error) {
	if stillReferenced == nil {
		return false, errors.New("secrets: DeleteIfUnreferenced requires a live-reference check")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[ref]; !ok {
		return false, nil
	}
	used, err := stillReferenced(ref)
	if err != nil {
		return false, fmt.Errorf("secrets: live-reference check: %w", err)
	}
	if used {
		return false, nil
	}
	return s.deleteLocked(ref)
}

// deleteLocked persists the vault without ref and commits the
// in-memory state. Caller holds s.mu and has already established
// that removing ref is safe.
func (s *Store) deleteLocked(ref string) (bool, error) {
	next := cloneEntries(s.entries)
	delete(next, ref)
	if err := s.persistLocked(next); err != nil {
		return false, err
	}
	s.entries = next
	delete(s.corrupt, ref)
	delete(s.pending, ref)
	return true, nil
}

// MarkCommitted records that the database transaction referencing
// ref has committed, ending its GC protection as a pending entry.
func (s *Store) MarkCommitted(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, ref)
}

// Refs snapshots the vault's reference set (with corruption flags)
// for consistency checks against the database.
func (s *Store) Refs() []RefInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RefInfo, 0, len(s.entries))
	for ref, e := range s.entries {
		out = append(out, RefInfo{Ref: ref, CreatedAt: e.CreatedAt, Corrupt: s.corrupt[ref]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// CollectGarbage deletes entries that are (a) not in live (the
// complete set of references held by the database), (b) not
// pending in this process, and (c) older than 24 hours. Entries
// referenced by ANY database row — including rows that share one
// reference — are in live and therefore untouchable. Returns the
// removed references.
func (s *Store) CollectGarbage(live map[string]bool, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var doomed []string
	for ref, e := range s.entries {
		if live[ref] || s.pending[ref] {
			continue
		}
		if now.Sub(e.CreatedAt) <= orphanAge {
			continue
		}
		doomed = append(doomed, ref)
	}
	if len(doomed) == 0 {
		return nil, nil
	}
	next := cloneEntries(s.entries)
	for _, ref := range doomed {
		delete(next, ref)
	}
	if err := s.persistLocked(next); err != nil {
		return nil, err
	}
	s.entries = next
	for _, ref := range doomed {
		delete(s.corrupt, ref)
	}
	sort.Strings(doomed)
	return doomed, nil
}

// persistLocked atomically replaces the vault file with the given
// entries: temp file (same directory) → fsync → restrict
// permissions → VERIFY the temp file's restrictions → snapshot the
// current good file to .bak (its temp file likewise restricted and
// verified first) → atomic replace → post-write verification.
// Every credential-bearing file is verified after it is
// restricted, and the final files are verified again after the
// replace: a RestrictFile success alone never counts as proof.
// Verification or content mismatches fail the whole persistence;
// the in-memory state is then left unchanged by the caller.
func (s *Store) persistLocked(entries map[string]entry) error {
	if HookBeforePersist != nil {
		if err := HookBeforePersist(); err != nil {
			return fmt.Errorf("secrets: persist aborted: %w", err)
		}
	}
	raw, err := json.Marshal(vaultFile{Version: vaultVersion, Entries: entries})
	if err != nil {
		return fmt.Errorf("secrets: encode vault: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := writeTempSynced(dir, filepath.Base(s.path)+".tmp-", raw)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := RestrictFile(tmp); err != nil {
		return fmt.Errorf("secrets: restrict temp vault: %w", err)
	}
	if err := verifyRestrictions(tmp); err != nil {
		return fmt.Errorf("secrets: verify temp vault permissions: %w", err)
	}
	// Snapshot the current good vault as .bak before replacing it,
	// so a later corruption of the new file can roll back one step.
	if current, err := readVaultFile(s.path); err == nil && len(current) >= 0 {
		if rawCur, err := os.ReadFile(s.path); err == nil {
			bakTmp, err := writeTempSynced(dir, filepath.Base(s.path)+".bak.tmp-", rawCur)
			if err != nil {
				return err
			}
			defer func() { _ = os.Remove(bakTmp) }()
			if err := RestrictFile(bakTmp); err != nil {
				return fmt.Errorf("secrets: restrict backup vault: %w", err)
			}
			if err := verifyRestrictions(bakTmp); err != nil {
				return fmt.Errorf("secrets: verify backup vault permissions: %w", err)
			}
			if err := replaceFile(bakTmp, s.path+".bak"); err != nil {
				return fmt.Errorf("secrets: snapshot backup vault: %w", err)
			}
		}
	}
	if err := replaceFile(tmp, s.path); err != nil {
		return fmt.Errorf("secrets: replace vault: %w", err)
	}
	syncDir(dir)
	if HookAfterReplace != nil {
		HookAfterReplace(s.path)
	}
	// Post-write content verification: the file on disk must hold
	// EXACTLY the intended state — version, the same reference
	// set, and per-entry CreatedAt and blob data. A matching entry
	// COUNT alone proves nothing.
	if err := verifyWrittenVault(s.path, entries); err != nil {
		return err
	}
	// Post-replace permission verification of the final files.
	if err := verifyRestrictions(s.path); err != nil {
		return fmt.Errorf("secrets: verify final vault permissions: %w", err)
	}
	if _, err := os.Stat(s.path + ".bak"); err == nil {
		if err := verifyRestrictions(s.path + ".bak"); err != nil {
			return fmt.Errorf("secrets: verify final backup permissions: %w", err)
		}
	}
	return nil
}

// verifyWrittenVault reads the vault file at path and proves its
// content equals want exactly: same format version, no missing
// entry, no extra entry, and every entry's CreatedAt and encrypted
// blob identical.
func verifyWrittenVault(path string, want map[string]entry) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("secrets: verify written vault: %w", err)
	}
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		return fmt.Errorf("secrets: verify written vault: %w", err)
	}
	if vf.Version != vaultVersion {
		return fmt.Errorf("secrets: written vault version is %d, expected %d", vf.Version, vaultVersion)
	}
	if len(vf.Entries) != len(want) {
		return fmt.Errorf("secrets: written vault holds %d entries, expected %d", len(vf.Entries), len(want))
	}
	for ref, w := range want {
		g, ok := vf.Entries[ref]
		if !ok {
			return fmt.Errorf("secrets: written vault is missing entry %s", ref)
		}
		if !g.CreatedAt.Equal(w.CreatedAt) {
			return fmt.Errorf("secrets: written vault entry %s has a different created_at", ref)
		}
		if g.Data != w.Data {
			return fmt.Errorf("secrets: written vault entry %s has different data", ref)
		}
	}
	for ref := range vf.Entries {
		if _, ok := want[ref]; !ok {
			return fmt.Errorf("secrets: written vault holds unexpected entry %s", ref)
		}
	}
	return nil
}

func readVaultFile(path string) (map[string]entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vf vaultFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		return nil, fmt.Errorf("secrets: parse vault: %w", err)
	}
	if vf.Version != vaultVersion {
		return nil, fmt.Errorf("secrets: unsupported vault version %d", vf.Version)
	}
	if vf.Entries == nil {
		vf.Entries = map[string]entry{}
	}
	return vf.Entries, nil
}

// writeTempSynced writes raw to a fresh temp file in dir and fsyncs
// it before closing, so a subsequent atomic replace never publishes
// unsynced content.
func writeTempSynced(dir, prefix string, raw []byte) (string, error) {
	f, err := os.CreateTemp(dir, prefix+"*")
	if err != nil {
		return "", fmt.Errorf("secrets: create temp vault: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("secrets: write temp vault: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("secrets: sync temp vault: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("secrets: close temp vault: %w", err)
	}
	return tmp, nil
}

// cleanStaleTemps removes leftover vault temp files from a crashed
// write. Best effort: the vault itself never depends on them.
func cleanStaleTemps(path string) {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	for _, pattern := range []string{base + ".tmp-*", base + ".bak.tmp-*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

func cloneEntries(in map[string]entry) map[string]entry {
	out := make(map[string]entry, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// newRef returns a random UUID-shaped opaque reference.
func newRef() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("secrets: generate reference: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

// Path returns the vault file path (for diagnostics).
func (s *Store) Path() string { return s.path }
