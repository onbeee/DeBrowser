package browserprobe

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// DER fixture builders: the tests below feed REAL DER-encoded
// X.509 Names through parseDERName — the same bytes a real
// certificate carries in its subject — not pre-converted strings.

func derLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	return []byte{0x81, byte(n)}
}

func derTLV(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, derLen(len(content))...)
	return append(out, content...)
}

func derOID(oid string) []byte {
	// Only the OIDs used in these fixtures: 2.5.4.x and 2.5.4.11.
	var arcs []int
	cur := 0
	part := ""
	for _, c := range oid + "." {
		if c == '.' {
			var n int
			for _, d := range part {
				n = n*10 + int(d-'0')
			}
			arcs = append(arcs, n)
			_ = cur
			part = ""
		} else {
			part += string(c)
		}
	}
	content := []byte{byte(40*arcs[0] + arcs[1])}
	for _, arc := range arcs[2:] {
		content = append(content, byte(arc))
	}
	return derTLV(0x06, content)
}

func derAttr(oid string, tag byte, value []byte) []byte {
	return derTLV(0x30, append(derOID(oid), derTLV(tag, value)...))
}

func derName(attrs ...[]byte) []byte {
	var rdns []byte
	for _, a := range attrs {
		rdns = append(rdns, derTLV(0x31, a)...)
	}
	return derTLV(0x30, rdns)
}

func bmpBytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(out[2*i:], c)
	}
	return out
}

func orgOf(t *testing.T, der []byte) string {
	t.Helper()
	attrs, err := parseDERName(der)
	if err != nil {
		t.Fatalf("parseDERName: %v", err)
	}
	org, ok := organizationName(attrs)
	if !ok {
		t.Fatal("no organization attribute parsed")
	}
	return org
}

func TestParseDERNameStringTypes(t *testing.T) {
	cases := []struct {
		name string
		der  []byte
		want string
	}{
		{"PrintableString org", derName(
			derAttr("2.5.4.3", tagPrintableString, []byte("Google LLC")),
			derAttr(oidOrganizationName, tagPrintableString, []byte("Google LLC")),
		), "Google LLC"},
		{"UTF8String org with comma", derName(
			derAttr(oidOrganizationName, tagUTF8String, []byte("Brave Software, Inc.")),
		), "Brave Software, Inc."},
		{"BMPString org", derName(
			derAttr(oidOrganizationName, tagBMPString, bmpBytes("Google LLC")),
		), "Google LLC"},
		{"TeletexString org", derName(
			derAttr(oidOrganizationName, tagTeletexString, []byte("Example Org")),
		), "Example Org"},
		{"UTF8String non-ASCII org", derName(
			derAttr(oidOrganizationName, tagUTF8String, []byte("Göogle Ünïcode LLC")),
		), "Göogle Ünïcode LLC"},
		{"BMPString non-ASCII org", derName(
			derAttr(oidOrganizationName, tagBMPString, bmpBytes("Göogle Ünïcode LLC")),
		), "Göogle Ünïcode LLC"},
		{"special characters preserved", derName(
			derAttr(oidOrganizationName, tagUTF8String, []byte(`Widgets "R" Us, LLC = Intl`)),
		), `Widgets "R" Us, LLC = Intl`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := orgOf(t, tc.der); got != tc.want {
				t.Fatalf("org = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseDERNameOIDMustMatchExactly(t *testing.T) {
	// An OU (2.5.4.11) whose value looks like an organization must
	// NOT be taken for the organization attribute.
	der := derName(derAttr("2.5.4.11", tagPrintableString, []byte("Google LLC")))
	attrs, err := parseDERName(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := organizationName(attrs); ok {
		t.Fatal("OU value must not be reported as the organization")
	}
}

func TestParseDERNameFailures(t *testing.T) {
	good := derName(derAttr(oidOrganizationName, tagPrintableString, []byte("Google LLC")))
	cases := map[string][]byte{
		"truncated":            good[:len(good)-3],
		"trailing garbage":     append(append([]byte{}, good...), 0x00),
		"indefinite length":    {0x30, 0x80, 0x00, 0x00},
		"wrong outer tag":      derTLV(0x31, []byte{}),
		"org value wrong type": derName(derAttr(oidOrganizationName, 0x30, []byte{0x05, 0x00})),
		"org invalid UTF8":     derName(derAttr(oidOrganizationName, tagUTF8String, []byte{0xFF, 0xFE})),
		"org odd BMP length":   derName(derAttr(oidOrganizationName, tagBMPString, []byte{0x00})),
		"empty":                {},
	}
	for name, der := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDERName(der); err == nil {
				t.Fatal("malformed or undecodable name must fail closed")
			}
		})
	}
}
