package browserprobe

import (
	"testing"
)

// Synthetic certificates for signer-matching tests. No real
// browser files are involved: the decision logic under test is
// exactly the logic the Windows layer feeds with Crypt32 data.
var (
	issuerGoogle = []byte{0x30, 0x0a, 'G', 'o', 'o', 'g', 'l', 'e'}
	issuerBrave  = []byte{0x30, 0x0a, 'B', 'r', 'a', 'v', 'e'}
	issuerOther  = []byte{0x30, 0x0a, 'O', 't', 'h', 'e', 'r'}
)

func cand(issuer, serial, encoded []byte, org string) certCandidate {
	c := certCandidate{Issuer: issuer, SerialNumber: serial, Encoded: encoded}
	if org != "" {
		c.Subject = []nameAttribute{
			{OID: "2.5.4.3", Value: "Some CN"},
			{OID: oidOrganizationName, Value: org},
			{OID: "2.5.4.6", Value: "US"},
		}
	}
	return c
}

func TestSignerPublisherSingleCert(t *testing.T) {
	pub, err := signerPublisherFromCandidates(issuerGoogle, []byte{1, 2, 3},
		[]certCandidate{cand(issuerGoogle, []byte{1, 2, 3}, []byte("cert-a"), "Google LLC")})
	if err != nil || pub != "Google LLC" {
		t.Fatalf("got %q, %v", pub, err)
	}
}

func TestSignerPublisherSignerIsNotFirst(t *testing.T) {
	cands := []certCandidate{
		cand(issuerOther, []byte{9}, []byte("cert-x"), "Unrelated Corp"),
		cand(issuerBrave, []byte{7, 7}, []byte("cert-b"), "Brave Software, Inc."),
		cand(issuerGoogle, []byte{1}, []byte("cert-g"), "Google LLC"),
	}
	pub, err := signerPublisherFromCandidates(issuerBrave, []byte{7, 7}, cands)
	if err != nil || pub != "Brave Software, Inc." {
		t.Fatalf("got %q, %v — must pick the actual signer, not the first cert", pub, err)
	}
}

func TestSignerPublisherFirstCertGoogleSignerNotGoogle(t *testing.T) {
	// The trap this fix exists for: the store's FIRST certificate
	// says Google, but the actual signer is someone else. The
	// answer must be the signer's publisher, never Google's.
	cands := []certCandidate{
		cand(issuerGoogle, []byte{1}, []byte("cert-g"), "Google LLC"),
		cand(issuerOther, []byte{2}, []byte("cert-o"), "Other Software Ltd"),
	}
	pub, err := signerPublisherFromCandidates(issuerOther, []byte{2}, cands)
	if err != nil || pub != "Other Software Ltd" {
		t.Fatalf("got %q, %v — publisher must be the signer's, not the first certificate's", pub, err)
	}
}

func TestSignerPublisherFirstCertBraveSignerNotBrave(t *testing.T) {
	cands := []certCandidate{
		cand(issuerBrave, []byte{1}, []byte("cert-b"), "Brave Software, Inc."),
		cand(issuerGoogle, []byte{2}, []byte("cert-g"), "Google LLC"),
	}
	pub, err := signerPublisherFromCandidates(issuerGoogle, []byte{2}, cands)
	if err != nil || pub != "Google LLC" {
		t.Fatalf("got %q, %v", pub, err)
	}
}

func TestSignerPublisherUnmatchable(t *testing.T) {
	cands := []certCandidate{cand(issuerGoogle, []byte{1}, []byte("cert-g"), "Google LLC")}
	if _, err := signerPublisherFromCandidates(issuerGoogle, []byte{99}, cands); err == nil {
		t.Fatal("serial mismatch must fail")
	}
	if _, err := signerPublisherFromCandidates(issuerBrave, []byte{1}, cands); err == nil {
		t.Fatal("issuer mismatch must fail")
	}
	if _, err := signerPublisherFromCandidates(issuerGoogle, []byte{1}, nil); err == nil {
		t.Fatal("empty store must fail")
	}
}

func TestSignerPublisherMissingSignerID(t *testing.T) {
	cands := []certCandidate{cand(issuerGoogle, []byte{1}, []byte("cert-g"), "Google LLC")}
	if _, err := signerPublisherFromCandidates(nil, []byte{1}, cands); err == nil {
		t.Fatal("missing issuer must fail")
	}
	if _, err := signerPublisherFromCandidates(issuerGoogle, nil, cands); err == nil {
		t.Fatal("missing serial must fail")
	}
}

func TestSignerPublisherAmbiguousDifferentCerts(t *testing.T) {
	// Two DIFFERENT encoded certificates with the same issuer and
	// serial: the signer cannot be uniquely identified.
	cands := []certCandidate{
		cand(issuerGoogle, []byte{1}, []byte("cert-a"), "Google LLC"),
		cand(issuerGoogle, []byte{1}, []byte("cert-b"), "Google LLC"),
	}
	if _, err := signerPublisherFromCandidates(issuerGoogle, []byte{1}, cands); err == nil {
		t.Fatal("ambiguous signer match must fail")
	}
	// Byte-identical duplicates of the SAME certificate are fine.
	cands[1].Encoded = []byte("cert-a")
	if _, err := signerPublisherFromCandidates(issuerGoogle, []byte{1}, cands); err != nil {
		t.Fatalf("identical duplicates must be accepted: %v", err)
	}
}

func TestSignerPublisherMissingOrganization(t *testing.T) {
	c := cand(issuerGoogle, []byte{1}, []byte("cert-a"), "")
	c.Subject = []nameAttribute{{OID: "2.5.4.3", Value: "No Org Here"}}
	if _, err := signerPublisherFromCandidates(issuerGoogle, []byte{1}, []certCandidate{c}); err == nil {
		t.Fatal("missing O= attribute must fail")
	}
}

func TestOrganizationSpecialCharacters(t *testing.T) {
	// Attribute values arrive pre-decoded, so commas, quotes and
	// '=' inside a value survive verbatim (the old implementation
	// split a rendered name string on commas and would have
	// mangled these).
	attrs := []nameAttribute{
		{OID: "2.5.4.3", Value: "ACME, Inc."},
		{OID: oidOrganizationName, Value: `Widgets "R" Us, LLC = Intl`},
	}
	org, ok := organizationName(attrs)
	if !ok || org != `Widgets "R" Us, LLC = Intl` {
		t.Fatalf("got %q, %v", org, ok)
	}
}

func TestSerialNumbersEqualTrailingZeros(t *testing.T) {
	if !serialNumbersEqual([]byte{1, 2, 0, 0}, []byte{1, 2}) {
		t.Fatal("trailing zero bytes must be insignificant")
	}
	if serialNumbersEqual([]byte{1, 2}, []byte{1, 3}) {
		t.Fatal("different serials must not compare equal")
	}
}

// --- enumeration contract (enumCollect) ---------------------------

type fakeEnum struct {
	items    []int
	errAt    int // index at which next() reports an error; -1 = never
	consumed map[int]bool
	released map[int]bool
	pending  int
	hasPend  bool
	calls    int
}

func newFakeEnum(items []int, errAt int) *fakeEnum {
	return &fakeEnum{items: items, errAt: errAt, consumed: map[int]bool{}, released: map[int]bool{}}
}

// next models CertEnumCertificatesInStore ownership: calling it
// consumes the previously returned handle.
func (f *fakeEnum) next() (int, bool, error) {
	if f.hasPend {
		f.consumed[f.pending] = true
		f.hasPend = false
	}
	idx := f.calls
	f.calls++
	if idx == f.errAt {
		return 0, false, errFakeEnum
	}
	if idx >= len(f.items) {
		return 0, false, nil // documented normal end
	}
	f.pending = f.items[idx]
	f.hasPend = true
	return f.pending, true, nil
}

func (f *fakeEnum) release(h int) {
	f.released[h] = true
	if f.hasPend && f.pending == h {
		f.hasPend = false
	}
}

var errFakeEnum = &fakeError{"enumeration exploded"}

type fakeError struct{ s string }

func (e *fakeError) Error() string { return e.s }

func intCand(h int) (certCandidate, error) {
	return certCandidate{Encoded: []byte{byte(h)}}, nil
}

func TestEnumCollectNormalEnd(t *testing.T) {
	f := newFakeEnum([]int{1, 2, 3}, -1)
	got, err := enumCollect(f.next, intCand, f.release)
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d candidates, err %v", len(got), err)
	}
	// Every handle accounted for exactly once: all three consumed
	// by subsequent next() calls (including the terminating one),
	// none released by enumCollect, none leaked.
	for _, h := range []int{1, 2, 3} {
		if !f.consumed[h] {
			t.Fatalf("handle %d was never consumed", h)
		}
		if f.released[h] {
			t.Fatalf("handle %d released on the success path", h)
		}
	}
	if f.hasPend {
		t.Fatal("a handle is still pending after normal end")
	}
}

func TestEnumCollectUnexpectedErrorDiscardsPartial(t *testing.T) {
	f := newFakeEnum([]int{1, 2, 3}, 2) // error when fetching the 3rd
	got, err := enumCollect(f.next, intCand, f.release)
	if err == nil {
		t.Fatal("unexpected enumeration error must fail the collection")
	}
	if got != nil {
		t.Fatalf("partial candidate set must be discarded, got %d", len(got))
	}
}

func TestEnumCollectConvertFailureReleasesInFlight(t *testing.T) {
	f := newFakeEnum([]int{1, 2, 3}, -1)
	convert := func(h int) (certCandidate, error) {
		if h == 2 {
			return certCandidate{}, errFakeEnum
		}
		return intCand(h)
	}
	got, err := enumCollect(f.next, convert, f.release)
	if err == nil || got != nil {
		t.Fatalf("got %v, err %v — convert failure must abort with no result", got, err)
	}
	// Handle 1 was consumed by the second next() call; handle 2
	// (in flight when convert failed) must be released exactly
	// once by enumCollect; nothing may remain pending.
	if !f.consumed[1] {
		t.Fatal("handle 1 should have been consumed")
	}
	if !f.released[2] {
		t.Fatal("in-flight handle 2 was not released on the early return")
	}
	if f.hasPend {
		t.Fatal("a handle is still pending after the early return")
	}
}
