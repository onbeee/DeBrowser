//go:build windows

package browserprobe

import (
	"fmt"
	"os"
	"strings"
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

	procCertNameToStr  = crypt32DLL.NewProc("CertNameToStrW")
	procWinVerifyTrust = wintrustDLL.NewProc("WinVerifyTrust")
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
	certX500NameStr                      = 3
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

// signerPublisher extracts the organization (O=) of the embedded
// signer's certificate subject.
func signerPublisher(pathUTF16 *uint16) (string, error) {
	var hStore windows.Handle
	err := windows.CryptQueryObject(
		certQueryObjectFile,
		unsafe.Pointer(pathUTF16),
		certQueryContentFlagPKCS7SignedEmbed,
		0, 0, nil, nil, nil, &hStore, nil, nil)
	if err != nil || hStore == 0 {
		return "", fmt.Errorf("open embedded signature store failed: %w", err)
	}
	defer func() { _ = windows.CertCloseStore(hStore, 0) }()
	cert, err := windows.CertEnumCertificatesInStore(hStore, nil)
	if err != nil || cert == nil {
		return "", fmt.Errorf("no signer certificate found: %w", err)
	}
	defer func() { _ = windows.CertFreeCertificateContext(cert) }()
	if cert.CertInfo == nil {
		return "", fmt.Errorf("signer certificate has no info block")
	}
	subject := &cert.CertInfo.Subject
	n, _, _ := procCertNameToStr.Call(
		1, // X509_ASN_ENCODING
		uintptr(unsafe.Pointer(subject)),
		certX500NameStr, 0, 0, 0)
	if n <= 1 {
		return "", fmt.Errorf("read signer name failed")
	}
	buf := make([]uint16, n)
	procCertNameToStr.Call(
		1, uintptr(unsafe.Pointer(subject)), certX500NameStr, 0,
		uintptr(unsafe.Pointer(&buf[0])), n)
	name := windows.UTF16ToString(buf)
	return parseOrganization(name), nil
}

// parseOrganization extracts the O= component of an X.500 name
// string like "CN=Google LLC, O=Google LLC, C=US".
func parseOrganization(x500 string) string {
	for _, part := range strings.Split(x500, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "O=") {
			return strings.Trim(strings.TrimPrefix(part, "O="), `"`)
		}
	}
	return ""
}
