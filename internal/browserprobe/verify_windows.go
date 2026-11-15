//go:build windows

package browserprobe

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows file verification: product/version from the file's
// version resource (version.dll) and Authenticode status plus
// signer publisher from wintrust.dll / crypt32.dll. All calls are
// read-only inspections — the file is never executed.

var (
	versionDLL  = windows.NewLazySystemDLL("version.dll")
	crypt32DLL  = windows.NewLazySystemDLL("crypt32.dll")
	wintrustDLL = windows.NewLazySystemDLL("wintrust.dll")

	procGetFileVersionInfoSize = versionDLL.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfo     = versionDLL.NewProc("GetFileVersionInfoW")
	procVerQueryValue          = versionDLL.NewProc("VerQueryValueW")

	procWinVerifyTrust = wintrustDLL.NewProc("WinVerifyTrust")

	procCryptMsgGetParam    = crypt32DLL.NewProc("CryptMsgGetParam")
	procCryptMsgClose       = crypt32DLL.NewProc("CryptMsgClose")
	procCryptDecodeObjectEx = crypt32DLL.NewProc("CryptDecodeObjectEx")
	procCertRDNValueToStr   = crypt32DLL.NewProc("CertRDNValueToStrW")

	kernel32DLL   = windows.NewLazySystemDLL("kernel32.dll")
	procLocalFree = kernel32DLL.NewProc("LocalFree")
)

// NewInspector returns the Windows inspector.
func NewInspector() Inspector { return windowsInspector{} }

type windowsInspector struct{}

// Exists reports whether path is an existing regular file.
func (windowsInspector) Exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// --- Version resource ----------------------------------------------

func (windowsInspector) VersionInfo(path string) (VersionInfo, error) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return VersionInfo{}, err
	}
	size, _, _ := procGetFileVersionInfoSize.Call(uintptr(unsafe.Pointer(p16)), 0)
	if size == 0 {
		return VersionInfo{}, fmt.Errorf("no version resource in %s", path)
	}
	buf := make([]byte, size)
	r, _, _ := procGetFileVersionInfo.Call(uintptr(unsafe.Pointer(p16)), 0, size, uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return VersionInfo{}, fmt.Errorf("read version resource of %s failed", path)
	}
	query := func(sub string) string {
		sp, err := windows.UTF16PtrFromString(sub)
		if err != nil {
			return ""
		}
		var ptr unsafe.Pointer
		var length uint32
		r, _, _ := procVerQueryValue.Call(
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(sp)),
			uintptr(unsafe.Pointer(&ptr)),
			uintptr(unsafe.Pointer(&length)))
		if r == 0 || ptr == nil || length == 0 {
			return ""
		}
		u16 := unsafe.Slice((*uint16)(ptr), length)
		return windows.UTF16ToString(u16)
	}
	// Language/codepage pairs from the translation table; fall back
	// to US English / Unicode.
	langCP := "040904b0"
	var tptr unsafe.Pointer
	var tlen uint32
	tsp, _ := windows.UTF16PtrFromString(`\VarFileInfo\Translation`)
	if r, _, _ := procVerQueryValue.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(tsp)),
		uintptr(unsafe.Pointer(&tptr)),
		uintptr(unsafe.Pointer(&tlen))); r != 0 && tptr != nil && tlen >= 4 {
		b := unsafe.Slice((*byte)(tptr), 4)
		langCP = fmt.Sprintf("%04x%04x", uint32(b[0])|uint32(b[1])<<8, uint32(b[2])|uint32(b[3])<<8)
	}
	base := `\StringFileInfo\` + langCP + `\`
	product := query(base + "ProductName")
	version := query(base + "FileVersion")
	if product == "" && version == "" {
		return VersionInfo{}, fmt.Errorf("version resource of %s has no product or version", path)
	}
	return VersionInfo{ProductName: product, FileVersion: version}, nil
}

// --- Authenticode ----------------------------------------------------

// wintrust structures (x86-64 layout).
type wintrustFileInfo struct {
	cbStruct       uint32
	pcwszFilePath  *uint16
	hFile          uintptr
	pgKnownSubject *windows.GUID
}

type wintrustData struct {
	cbStruct            uint32
	pPolicyCallbackData uintptr
	pSIPClientData      uintptr
	dwUIAction          uint32
	fdwRevocationChecks uint32
	dwUnionChoice       uint32
	pFile               *wintrustFileInfo
	dwStateAction       uint32
	hWVTStateData       uintptr
	pwszURLReference    *uint16
	dwProvFlags         uint32
	dwUIContext         uint32
	pSignatureSettings  uintptr
}

var winTrustActionGenericVerifyV2 = windows.GUID{
	Data1: 0x00AAC56B, Data2: 0xCD44, Data3: 0x11D0,
	Data4: [8]byte{0x8C, 0xC2, 0x00, 0xC0, 0x4F, 0xC2, 0x95, 0xEE},
}

const (
	wtdUINone                            = 2
	wtdRevokeNone                        = 0
	wtdChoiceFile                        = 1
	wtdStateVerify                       = 1
	wtdStateClose                        = 2
	trustENoSignature                    = 0x800B0100
	certQueryObjectFile                  = 1
	certQueryContentFlagPKCS7SignedEmbed = 1 << 10
	cmsgSignerCountParam                 = 5
	cmsgSignerInfoParam                  = 6
	x509Name                             = 7 // MAKEINTRESOURCE(7)
	cryptDecodeAllocFlag                 = 0x8000
	x509ASNEncoding                      = 1
)

func (windowsInspector) Signature(path string) (Signature, error) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Signature{}, err
	}
	fi := wintrustFileInfo{pcwszFilePath: p16}
	fi.cbStruct = uint32(unsafe.Sizeof(fi))
	data := wintrustData{
		dwUIAction:          wtdUINone,
		fdwRevocationChecks: wtdRevokeNone,
		dwUnionChoice:       wtdChoiceFile,
		pFile:               &fi,
		dwStateAction:       wtdStateVerify,
	}
	data.cbStruct = uint32(unsafe.Sizeof(data))
	r, _, _ := procWinVerifyTrust.Call(
		uintptr(0xFFFFFFFFFFFFFFFF), // INVALID_HANDLE_VALUE: no UI window
		uintptr(unsafe.Pointer(&winTrustActionGenericVerifyV2)),
		uintptr(unsafe.Pointer(&data)))
	// Always release the state data.
	data.dwStateAction = wtdStateClose
	_, _, _ = procWinVerifyTrust.Call(
		uintptr(0xFFFFFFFFFFFFFFFF),
		uintptr(unsafe.Pointer(&winTrustActionGenericVerifyV2)),
		uintptr(unsafe.Pointer(&data)))
	switch uint32(r) {
	case 0:
		pub, err := signerPublisher(p16)
		if err != nil {
			// Signature is valid but the publisher could not be
			// read: treat as invalid rather than trusting it.
			return Signature{Status: "invalid"}, nil
		}
		return Signature{Status: "valid", Publisher: pub}, nil
	case trustENoSignature:
		return Signature{Status: "unsigned"}, nil
	default:
		return Signature{Status: "invalid"}, nil
	}
}

// dataBlob mirrors the Crypt32 CRYPT_DATA_BLOB layout (x64).
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func blobBytes(b dataBlob) []byte {
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

// cmsgSignerInfoHead mirrors the head of CMSG_SIGNER_INFO — the
// only fields signer identification needs (dwVersion, Issuer,
// SerialNumber); the remaining fields are not read.
type cmsgSignerInfoHead struct {
	dwVersion    uint32
	issuer       dataBlob
	serialNumber dataBlob
}

// signerPublisher returns the organization (O=) of the certificate
// that ACTUALLY signed the file. It reads the signer identifier
// (Issuer + SerialNumber) from the PKCS#7 signer info via
// CryptMsgGetParam, then selects the one certificate in the
// embedded store matching that identifier (see signer.go). The
// first certificate in the store is never trusted on its own, and
// any ambiguity or missing data is an error.
func signerPublisher(pathUTF16 *uint16) (string, error) {
	var hStore windows.Handle
	var hMsg windows.Handle
	err := windows.CryptQueryObject(
		certQueryObjectFile,
		unsafe.Pointer(pathUTF16),
		certQueryContentFlagPKCS7SignedEmbed,
		0, 0, nil, nil, nil, &hStore, &hMsg, nil)
	if err != nil || hStore == 0 || hMsg == 0 {
		return "", fmt.Errorf("open embedded signature failed: %w", err)
	}
	defer func() { _ = windows.CertCloseStore(hStore, 0) }()
	defer func() { _, _, _ = procCryptMsgClose.Call(uintptr(hMsg)) }()

	issuer, serial, err := msgSignerID(hMsg)
	if err != nil {
		return "", err
	}
	cands, err := storeCandidates(hStore)
	if err != nil {
		return "", err
	}
	return signerPublisherFromCandidates(issuer, serial, cands)
}

// msgSignerID extracts the signer identifier of the message's
// single signer.
func msgSignerID(hMsg windows.Handle) (issuer, serial []byte, err error) {
	var count uint32
	var size uint32
	r, _, callErr := procCryptMsgGetParam.Call(uintptr(hMsg), cmsgSignerCountParam, 0,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return nil, nil, fmt.Errorf("read signer count failed: %w", callErr)
	}
	if count != 1 {
		return nil, nil, fmt.Errorf("signature has %d signers, want exactly 1", count)
	}
	size = 0
	r, _, callErr = procCryptMsgGetParam.Call(uintptr(hMsg), cmsgSignerInfoParam, 0, 0,
		uintptr(unsafe.Pointer(&size)))
	if r == 0 || size < uint32(unsafe.Sizeof(cmsgSignerInfoHead{})) {
		return nil, nil, fmt.Errorf("size signer info failed: %w", callErr)
	}
	buf := make([]byte, size)
	r, _, callErr = procCryptMsgGetParam.Call(uintptr(hMsg), cmsgSignerInfoParam, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return nil, nil, fmt.Errorf("read signer info failed: %w", callErr)
	}
	head := (*cmsgSignerInfoHead)(unsafe.Pointer(&buf[0]))
	return blobBytes(head.issuer), blobBytes(head.serialNumber), nil
}

// cryptENoMoreFiles is CRYPT_E_NO_MORE_FILES: the documented
// error CertEnumCertificatesInStore reports when enumeration is
// complete. ERROR_NO_MORE_ITEMS (259) is accepted as well. Any
// other error is unexpected and aborts verification — a partial
// enumeration could hide a second certificate matching the signer
// identifier and defeat the ambiguity check.
const cryptENoMoreFiles = 0x8009200D

func isEnumEnd(err error) bool {
	if err == nil {
		return true
	}
	var errno windows.Errno
	if errors.As(err, &errno) {
		return errno == windows.Errno(cryptENoMoreFiles) || errno == windows.ERROR_NO_MORE_ITEMS
	}
	return false
}

// storeCandidates enumerates the embedded certificate store into
// matchable candidates with decoded subjects, via enumCollect
// (see signer.go for the fail-closed contract):
//
//   - Only the documented end-of-enumeration outcome completes
//     the collection; any other Crypt32 error aborts it and the
//     partial result is discarded.
//   - Context ownership follows the CertEnumCertificatesInStore
//     contract: the API consumes the previous context on every
//     call, so the closure clears its pending pointer after each
//     call; if converting the current certificate fails, that
//     context has not been consumed yet and enumCollect releases
//     it exactly once through the release callback.
func storeCandidates(hStore windows.Handle) ([]certCandidate, error) {
	var pending *windows.CertContext
	next := func() (*windows.CertContext, bool, error) {
		cert, err := windows.CertEnumCertificatesInStore(hStore, pending)
		pending = nil // consumed by the API in every outcome
		if cert == nil {
			if isEnumEnd(err) {
				return nil, false, nil
			}
			return nil, false, fmt.Errorf("enumerate signature store failed: %w", err)
		}
		pending = cert
		return cert, true, nil
	}
	convert := func(cert *windows.CertContext) (certCandidate, error) {
		if cert.CertInfo == nil {
			return certCandidate{}, fmt.Errorf("certificate in signature store has no info block")
		}
		info := cert.CertInfo
		cand := certCandidate{
			Issuer:       blobBytes(dataBlob{cbData: info.Issuer.Size, pbData: info.Issuer.Data}),
			SerialNumber: blobBytes(dataBlob{cbData: info.SerialNumber.Size, pbData: info.SerialNumber.Data}),
		}
		if cert.EncodedCert != nil && cert.Length > 0 {
			cand.Encoded = make([]byte, cert.Length)
			copy(cand.Encoded, unsafe.Slice(cert.EncodedCert, cert.Length))
		}
		attrs, derr := decodeSubjectName(info.Subject.Data, info.Subject.Size)
		if derr != nil {
			return certCandidate{}, derr
		}
		cand.Subject = attrs
		return cand, nil
	}
	release := func(cert *windows.CertContext) {
		if pending == cert {
			pending = nil
		}
		_ = windows.CertFreeCertificateContext(cert)
	}
	out, err := enumCollect(next, convert, release)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("embedded signature store contains no certificates")
	}
	return out, nil
}

// Raw CERT_NAME_INFO decode structures (x64 layout).
type certRDNAttrRaw struct {
	pszObjId    *byte
	dwValueType uint32
	value       dataBlob
}

type certRDNRaw struct {
	cRDNAttr  uint32
	rgRDNAttr *certRDNAttrRaw
}

type certNameInfoRaw struct {
	cRDN  uint32
	rgRDN *certRDNRaw
}

// rdnStringValueTypes are the CERT_RDN character-string value
// types this code knows how to convert. Anything else (binary
// types are filtered by the caller; unknown types) fails closed
// rather than being guessed at.
var rdnStringValueTypes = map[uint32]bool{
	3:  true, // CERT_RDN_NUMERIC_STRING
	4:  true, // CERT_RDN_PRINTABLE_STRING
	5:  true, // CERT_RDN_TELETEX_STRING
	6:  true, // CERT_RDN_VIDEOTEX_STRING
	7:  true, // CERT_RDN_IA5_STRING
	8:  true, // CERT_RDN_GRAPHIC_STRING
	9:  true, // CERT_RDN_VISIBLE_STRING
	10: true, // CERT_RDN_GENERAL_STRING
	11: true, // CERT_RDN_UNIVERSAL_STRING
	12: true, // CERT_RDN_INT4_STRING
	13: true, // CERT_RDN_BMP_STRING (= CERT_RDN_UNICODE_STRING)
	14: true, // CERT_RDN_UTF8_STRING
}

// rdnValueToString converts one RDN attribute value to a Unicode
// string with CertRDNValueToStrW, which converts according to the
// attribute's own dwValueType (PrintableString, UTF8String,
// BMPString/UnicodeString, TeletexString, IA5String, …). The
// input blob is the value in the decoded form produced by
// CryptDecodeObjectEx — the exact form the API consumes (character
// strings as UTF-16LE, with the documented exception that
// CERT_RDN_UTF8_STRING values remain UTF-8 bytes). No encoding is
// assumed by this code and no length is guessed: the buffer size
// comes from the API itself (first call). A failed conversion is
// an error — callers fail closed.
func rdnValueToString(valueType uint32, b dataBlob) (string, error) {
	if !rdnStringValueTypes[valueType] {
		return "", fmt.Errorf("unsupported RDN value type %d", valueType)
	}
	if b.cbData == 0 || b.pbData == nil {
		return "", fmt.Errorf("empty RDN attribute value (type %d)", valueType)
	}
	n, _, _ := procCertRDNValueToStr.Call(
		uintptr(valueType), uintptr(unsafe.Pointer(&b)), 0, 0)
	if n == 0 {
		return "", fmt.Errorf("CertRDNValueToStrW sizing failed for value type %d", valueType)
	}
	buf := make([]uint16, n)
	m, _, _ := procCertRDNValueToStr.Call(
		uintptr(valueType), uintptr(unsafe.Pointer(&b)),
		uintptr(unsafe.Pointer(&buf[0])), n)
	if m == 0 {
		return "", fmt.Errorf("CertRDNValueToStrW failed for value type %d", valueType)
	}
	return windows.UTF16ToString(buf), nil
}

// decodeSubjectName decodes a DER X.509 name via CryptDecodeObjectEx
// (X509_NAME) into individual attributes — the reliable X.500
// attribute parsing path; no rendered-name string splitting is
// involved. Each attribute VALUE is converted by CertRDNValueToStrW
// according to its own value type (see rdnValueToString); any
// conversion failure aborts the decode (fail closed). Binary value
// types (1 = encoded blob, 2 = octet string) carry no text and are
// recorded with an empty value — an organization attribute of a
// binary type therefore yields an empty organization, which the
// signer-matching layer rejects.
func decodeSubjectName(data *byte, size uint32) ([]nameAttribute, error) {
	if data == nil || size == 0 {
		return nil, fmt.Errorf("empty certificate subject")
	}
	var outPtr unsafe.Pointer
	var outSize uint32
	r, _, callErr := procCryptDecodeObjectEx.Call(
		x509ASNEncoding, x509Name,
		uintptr(unsafe.Pointer(data)), uintptr(size),
		cryptDecodeAllocFlag, 0,
		uintptr(unsafe.Pointer(&outPtr)), uintptr(unsafe.Pointer(&outSize)))
	if r == 0 || outPtr == nil {
		return nil, fmt.Errorf("decode certificate subject failed: %w", callErr)
	}
	defer func() { _, _, _ = procLocalFree.Call(uintptr(outPtr)) }()
	info := (*certNameInfoRaw)(outPtr)
	var attrs []nameAttribute
	rdns := unsafe.Slice(info.rgRDN, info.cRDN)
	for _, rdn := range rdns {
		rawAttrs := unsafe.Slice(rdn.rgRDNAttr, rdn.cRDNAttr)
		for _, ra := range rawAttrs {
			attr := nameAttribute{OID: cString(ra.pszObjId)}
			if ra.dwValueType != 1 && ra.dwValueType != 2 {
				v, verr := rdnValueToString(ra.dwValueType, ra.value)
				if verr != nil {
					return nil, verr
				}
				attr.Value = v
			}
			attrs = append(attrs, attr)
		}
	}
	return attrs, nil
}

func cString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + uintptr(n))) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}
