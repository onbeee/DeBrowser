//go:build windows

package browserprobe

import (
	"testing"
	"unicode/utf16"
	"unsafe"
)

// These tests exercise the REAL CertRDNValueToStrW conversion
// through rdnValueToString. They run on Windows (including the
// Windows CI runners). Inputs are value blobs in the decoded form
// a CERT_RDN_ATTR carries inside a decoded CERT_NAME_INFO — for
// character-string types that form is UTF-16LE text, and the API
// converts it according to each attribute's own dwValueType.

const (
	rdnNumericString   = 3
	rdnPrintableString = 4
	rdnTeletexString   = 5
	rdnIA5String       = 7
	rdnBMPString       = 13
	rdnUTF8String      = 14
)

func utf16Blob(s string) (dataBlob, []uint16) {
	u := utf16.Encode([]rune(s))
	if len(u) == 0 {
		return dataBlob{}, nil
	}
	return dataBlob{cbData: uint32(len(u) * 2), pbData: (*byte)(unsafe.Pointer(&u[0]))}, u
}

func TestRDNValueToStringRealConversion(t *testing.T) {
	cases := []struct {
		name      string
		valueType uint32
		value     string
	}{
		{"PrintableString org", rdnPrintableString, "Google LLC"},
		{"UTF8String org with comma", rdnUTF8String, "Brave Software, Inc."},
		{"BMPString org", rdnBMPString, "Google LLC"},
		{"TeletexString org", rdnTeletexString, "Example Org"},
		{"IA5String value", rdnIA5String, "admin@example.com"},
		{"NumericString value", rdnNumericString, "12345"},
		{"non-ASCII org", rdnUTF8String, "Göogle Ünïcode LLC"},
		{"commas quotes equals preserved", rdnPrintableString, `Widgets "R" Us, LLC = Intl`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob, keep := utf16Blob(tc.value)
			_ = keep // keep the backing array alive for the call
			got, err := rdnValueToString(tc.valueType, blob)
			if err != nil {
				t.Fatalf("rdnValueToString(type %d): %v", tc.valueType, err)
			}
			if got != tc.value {
				t.Fatalf("got %q, want %q", got, tc.value)
			}
		})
	}
}

func TestRDNValueToStringFailures(t *testing.T) {
	blob, keep := utf16Blob("Google LLC")
	_ = keep
	if _, err := rdnValueToString(0xFFFF, blob); err == nil {
		t.Fatal("an unsupported value type must fail, not guess an encoding")
	}
	if _, err := rdnValueToString(rdnPrintableString, dataBlob{}); err == nil {
		t.Fatal("an empty value blob must fail")
	}
}
