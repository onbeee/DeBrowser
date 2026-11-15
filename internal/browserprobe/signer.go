package browserprobe

import (
	"bytes"
	"fmt"
)

// Signer-certificate identification (platform-neutral decision
// logic; the Windows implementation in verify_windows.go feeds it
// real data from the Crypt32 PKCS#7 APIs).
//
// The publisher of a signed file must come from the certificate
// that ACTUALLY signed it — never from "the first certificate in
// the embedded store", which may be any certificate in the chain
// or an unrelated one bundled into the signature. The platform
// layer therefore extracts the signer identifier (Issuer +
// SerialNumber) from the PKCS#7 signer info and this code selects
// the one store certificate that matches it exactly.

// nameAttribute is one decoded X.500 attribute (OID + value) of a
// certificate subject. Values arrive already decoded by the
// platform layer, so characters like commas, quotes or '=' inside
// a value are preserved verbatim — no string splitting is involved
// anywhere in publisher extraction.
type nameAttribute struct {
	OID   string
	Value string
}

// oidOrganizationName is the X.500 organization attribute (O=).
const oidOrganizationName = "2.5.4.10"

// certCandidate is one certificate from a signature's embedded
// certificate store, reduced to what signer matching needs.
type certCandidate struct {
	Issuer       []byte // DER-encoded issuer name blob
	SerialNumber []byte // little-endian integer blob, as reported by Crypt32
	Encoded      []byte // the full encoded certificate (identity/dedup)
	Subject      []nameAttribute
}

// organizationName returns the O= value of a decoded subject, or
// false when the subject carries no organization attribute.
func organizationName(attrs []nameAttribute) (string, bool) {
	for _, a := range attrs {
		if a.OID == oidOrganizationName {
			return a.Value, true
		}
	}
	return "", false
}

// serialNumbersEqual compares two Crypt32 integer blobs. Both use
// the little-endian representation, so insignificant trailing zero
// bytes (the most significant end) are ignored.
func serialNumbersEqual(a, b []byte) bool {
	trim := func(x []byte) []byte {
		for len(x) > 0 && x[len(x)-1] == 0 {
			x = x[:len(x)-1]
		}
		return x
	}
	return bytes.Equal(trim(a), trim(b))
}

// selectSignerCert finds the one certificate in cands whose
// Issuer and SerialNumber exactly match the signer identifier
// taken from the PKCS#7 signer info. It fails when the signer
// identifier is missing, when nothing matches, or when two
// DIFFERENT certificates match (ambiguous — no publisher may be
// trusted then). Byte-identical duplicates of the same certificate
// count as one.
func selectSignerCert(issuer, serial []byte, cands []certCandidate) (certCandidate, error) {
	if len(issuer) == 0 || len(serial) == 0 {
		return certCandidate{}, fmt.Errorf("signer identifier is missing (issuer %d bytes, serial %d bytes)", len(issuer), len(serial))
	}
	var match *certCandidate
	for i := range cands {
		c := &cands[i]
		if !bytes.Equal(c.Issuer, issuer) || !serialNumbersEqual(c.SerialNumber, serial) {
			continue
		}
		if match == nil {
			match = c
			continue
		}
		if !bytes.Equal(match.Encoded, c.Encoded) {
			return certCandidate{}, fmt.Errorf("multiple different certificates match the signer identifier")
		}
	}
	if match == nil {
		return certCandidate{}, fmt.Errorf("no certificate in the signature store matches the signer identifier")
	}
	return *match, nil
}

// signerPublisherFromCandidates resolves the publisher (subject
// O=) of the actual signer certificate. Any failure — unmatchable
// signer, missing organization attribute — is an error: the
// caller must treat the file's publisher as unverifiable rather
// than fall back to some other certificate in the store.
func signerPublisherFromCandidates(issuer, serial []byte, cands []certCandidate) (string, error) {
	signer, err := selectSignerCert(issuer, serial, cands)
	if err != nil {
		return "", err
	}
	org, ok := organizationName(signer.Subject)
	if !ok || org == "" {
		return "", fmt.Errorf("signer certificate subject has no organization attribute")
	}
	return org, nil
}
