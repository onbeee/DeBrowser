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

func utf8Blob(s string) (dataBlob, []byte) {
	b := []byte(s)
	if len(b) == 0 {
		return dataBlob{}, nil
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}, b
}

func TestRDNValueToStringRealConversion(t *testing.T) {
	// Input blobs mirror the decoded form CryptDecodeObjectEx
	// produces inside a CERT_NAME_INFO: character strings are
	// UTF-16LE, EXCEPT CERT_RDN_UTF8_STRING values, which remain
	// UTF-8 bytes (the documented exception CertRDNValueToStrW
	// consumes).
	cases := []struct {
		name      string
		valueType uint32
		blob      func(string) (dataBlob, any)
		value     string
	}{
		{"PrintableString org", rdnPrintableString, utf16Arg, "Google LLC"},
		{"UTF8String org with comma", rdnUTF8String, utf8Arg, "Brave Software, Inc."},
		{"BMPString org", rdnBMPString, utf16Arg, "Google LLC"},
		{"TeletexString org", rdnTeletexString, utf16Arg, "Example Org"},
		{"IA5String value", rdnIA5String, utf16Arg, "admin@example.com"},
		{"NumericString value", rdnNumericString, utf16Arg, "12345"},
		{"non-ASCII org (UTF8)", rdnUTF8String, utf8Arg, "Göogle Ünïcode LLC"},
		{"non-ASCII org (BMP)", rdnBMPString, utf16Arg, "Göogle Ünïcode LLC"},
		{"commas quotes equals preserved", rdnPrintableString, utf16Arg, `Widgets "R" Us, LLC = Intl`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blob, keep := tc.blob(tc.value)
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

func utf16Arg(s string) (dataBlob, any) { b, k := utf16Blob(s); return b, k }
func utf8Arg(s string) (dataBlob, any)  { b, k := utf8Blob(s); return b, k }

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
