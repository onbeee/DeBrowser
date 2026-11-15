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

	procCryptMsgGetParam = crypt32DLL.NewProc("CryptMsgGetParam")
	procCryptMsgClose    = crypt32DLL.NewProc("CryptMsgClose")
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

// decodeSubjectName parses the DER-encoded subject name carried
// by a certificate (CERT_NAME_BLOB content) with the strict,
// platform-neutral DER parser in nameparse.go, which converts
// every X.500 string type by its own encoding rules. (The earlier
// CertRDNValueToStrW approach was abandoned after the real
// Crypt32, exercised on the Windows CI runners, returned
// first-character-only results for UTF-16LE-fed ASCII string
// types and byte-widened mojibake for UTF-8 input — see
// nameparse.go.)
func decodeSubjectName(data *byte, size uint32) ([]nameAttribute, error) {
	if data == nil || size == 0 {
		return nil, fmt.Errorf("empty certificate subject")
	}
	return parseDERName(blobBytes(dataBlob{cbData: size, pbData: data}))
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
