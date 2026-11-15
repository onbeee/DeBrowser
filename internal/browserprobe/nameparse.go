package browserprobe

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Strict DER parsing of an X.509 Name into X.500 attributes.
//
// Why not CertRDNValueToStrW: that API's input-form contract
// proved unreliable in practice — exercised against the real
// Crypt32 on the Windows CI runners, it returned only the first
// character for UTF-16LE-fed PrintableString/Teletex/IA5/Numeric
// values and byte-widened (mojibake) UTF-8 input instead of
// decoding it, so no single input form handled all required
// string types correctly. The subject blob carried by a
// certificate (CERT_NAME_BLOB) is plain DER, so this parser works
// from the one representation whose semantics are fully specified
// (X.680/X.690), converts every string type by its own encoding
// rules, and is testable with real DER fixtures on any platform.
//
// The parser is deliberately strict and fail-closed: definite
// lengths only, every length bounds-checked, no trailing garbage,
// and an organization attribute whose value type cannot be
// decoded is an error, never a guess.

const (
	tagUTF8String      = 0x0C
	tagNumericString   = 0x12
	tagPrintableString = 0x13
	tagTeletexString   = 0x14
	tagIA5String       = 0x16
	tagGraphicString   = 0x19
	tagVisibleString   = 0x1A
	tagGeneralString   = 0x1B
	tagUniversalString = 0x1C
	tagBMPString       = 0x1E
)

// derReader is a bounds-checked cursor over DER bytes.
type derReader struct {
	buf []byte
	pos int
}

func (r *derReader) readTLV(wantTag byte) ([]byte, error) {
	if r.pos+2 > len(r.buf) {
		return nil, fmt.Errorf("der: truncated TLV header")
	}
	tag := r.buf[r.pos]
	r.pos++
	if tag != wantTag {
		return nil, fmt.Errorf("der: tag 0x%02x, want 0x%02x", tag, wantTag)
	}
	length, err := r.readLength()
	if err != nil {
		return nil, err
	}
	if length > len(r.buf)-r.pos {
		return nil, fmt.Errorf("der: length %d exceeds remaining %d bytes", length, len(r.buf)-r.pos)
	}
	content := r.buf[r.pos : r.pos+length]
	r.pos += length
	return content, nil
}

func (r *derReader) readLength() (int, error) {
	if r.pos >= len(r.buf) {
		return 0, fmt.Errorf("der: truncated length")
	}
	b := r.buf[r.pos]
	r.pos++
	if b&0x80 == 0 {
		return int(b), nil
	}
	n := int(b & 0x7F)
	if n == 0 {
		return 0, fmt.Errorf("der: indefinite length is not valid DER")
	}
	if n > 4 || r.pos+n > len(r.buf) {
		return 0, fmt.Errorf("der: invalid long-form length")
	}
	length := 0
	for i := 0; i < n; i++ {
		length = length<<8 | int(r.buf[r.pos])
		r.pos++
	}
	return length, nil
}

func (r *derReader) atEnd() bool { return r.pos == len(r.buf) }

// decodeOID renders a DER OID content block in dotted form.
func decodeOID(content []byte) (string, error) {
	if len(content) == 0 {
		return "", fmt.Errorf("der: empty OID")
	}
	oid := fmt.Sprintf("%d.%d", content[0]/40, content[0]%40)
	if content[0] >= 80 { // arc0 is capped at 2 with a large arc1
		oid = fmt.Sprintf("2.%d", content[0]-80)
	}
	value := 0
	started := false
	for _, b := range content[1:] {
		value = value<<7 | int(b&0x7F)
		started = true
		if b&0x80 == 0 {
			oid += fmt.Sprintf(".%d", value)
			value = 0
			started = false
		}
	}
	if started {
		return "", fmt.Errorf("der: truncated OID arc")
	}
	return oid, nil
}

// derStringValue converts the content bytes of one DER string
// value to a Go string, strictly by the value's ASN.1 type.
func derStringValue(tag byte, content []byte) (string, error) {
	switch tag {
	case tagUTF8String:
		if !utf8.Valid(content) {
			return "", fmt.Errorf("der: invalid UTF-8 string value")
		}
		return string(content), nil
	case tagPrintableString, tagNumericString, tagIA5String,
		tagVisibleString, tagGeneralString, tagGraphicString:
		// Single-byte character sets (ASCII and close relatives):
		// one byte is one character.
		runes := make([]rune, len(content))
		for i, b := range content {
			runes[i] = rune(b)
		}
		return string(runes), nil
	case tagTeletexString:
		// T.61: for the printable repertoire used by real CAs this
		// is byte-per-character; escape/combining sequences are not
		// specially interpreted.
		runes := make([]rune, len(content))
		for i, b := range content {
			runes[i] = rune(b)
		}
		return string(runes), nil
	case tagBMPString:
		if len(content)%2 != 0 {
			return "", fmt.Errorf("der: odd-length BMPString")
		}
		u16 := make([]uint16, len(content)/2)
		for i := range u16 {
			u16[i] = uint16(content[2*i])<<8 | uint16(content[2*i+1])
		}
		return string(utf16.Decode(u16)), nil
	case tagUniversalString:
		if len(content)%4 != 0 {
			return "", fmt.Errorf("der: UniversalString length not a multiple of 4")
		}
		runes := make([]rune, len(content)/4)
		for i := range runes {
			runes[i] = rune(uint32(content[4*i])<<24 | uint32(content[4*i+1])<<16 |
				uint32(content[4*i+2])<<8 | uint32(content[4*i+3]))
			if !utf8.ValidRune(runes[i]) {
				return "", fmt.Errorf("der: invalid UniversalString code point")
			}
		}
		return string(runes), nil
	}
	return "", fmt.Errorf("der: unsupported string tag 0x%02x", tag)
}

// parseDERName parses a DER-encoded X.509 Name
// (SEQUENCE OF SET OF SEQUENCE { OID, value }) into attributes.
// An attribute with a non-string value type is recorded with an
// empty value, EXCEPT the organization attribute: an undecodable
// O= fails the whole parse (fail closed).
func parseDERName(der []byte) ([]nameAttribute, error) {
	top := &derReader{buf: der}
	nameContent, err := top.readTLV(0x30) // SEQUENCE
	if err != nil {
		return nil, fmt.Errorf("parse name: %w", err)
	}
	if !top.atEnd() {
		return nil, fmt.Errorf("parse name: trailing data after Name")
	}
	var attrs []nameAttribute
	rdns := &derReader{buf: nameContent}
	for !rdns.atEnd() {
		setContent, err := rdns.readTLV(0x31) // SET
		if err != nil {
			return nil, fmt.Errorf("parse name RDN: %w", err)
		}
		set := &derReader{buf: setContent}
		for !set.atEnd() {
			atvContent, err := set.readTLV(0x30) // SEQUENCE
			if err != nil {
				return nil, fmt.Errorf("parse name attribute: %w", err)
			}
			atv := &derReader{buf: atvContent}
			oidBytes, err := atv.readTLV(0x06) // OID
			if err != nil {
				return nil, fmt.Errorf("parse attribute OID: %w", err)
			}
			oid, err := decodeOID(oidBytes)
			if err != nil {
				return nil, err
			}
			if atv.pos >= len(atv.buf) {
				return nil, fmt.Errorf("parse attribute %s: missing value", oid)
			}
			valueTag := atv.buf[atv.pos]
			valueContent, err := atv.readTLV(valueTag)
			if err != nil {
				return nil, fmt.Errorf("parse attribute %s value: %w", oid, err)
			}
			if !atv.atEnd() {
				return nil, fmt.Errorf("parse attribute %s: trailing data", oid)
			}
			value, verr := derStringValue(valueTag, valueContent)
			if verr != nil {
				if oid == oidOrganizationName {
					return nil, fmt.Errorf("organization attribute cannot be decoded: %w", verr)
				}
				value = "" // non-string attribute of another type: no text
			}
			attrs = append(attrs, nameAttribute{OID: oid, Value: value})
		}
	}
	return attrs, nil
}
