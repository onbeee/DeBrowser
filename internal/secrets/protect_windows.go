//go:build windows

package secrets

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Windows credential protection via DPAPI (Data Protection API),
// loaded from crypt32.dll through the standard library only — the
// same LazyDLL style as internal/singleinstance's LockFileEx.
//
// Scope is CRYPTPROTECT_UI_FORBIDDEN with the CurrentUser default:
// the blob can be decrypted ONLY by the same Windows user on the
// same machine. That binding is a deliberate product property (see
// the package documentation): a vault copied to another machine or
// user fails decryption entry by entry, which the store surfaces as
// ErrCorrupt so the user is asked to re-enter credentials — the
// data is never silently dropped or downgraded.

var (
	crypt32              = syscall.NewLazyDLL("crypt32.dll")
	kernel32ForSecrets   = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect   = crypt32.NewProc("CryptUnprotectData")
	procLocalFreeSecrets = kernel32ForSecrets.NewProc("LocalFree")
	cryptprotectUIForbid = uintptr(0x1) // CRYPTPROTECT_UI_FORBIDDEN
	dpapiEntropyMaterial = []byte("DeBrowser/secrets/v1")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func makeBlob(data []byte) dataBlob {
	if len(data) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
}

func blobBytes(b dataBlob) []byte {
	if b.cbData == 0 || b.pbData == nil {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, int(b.cbData)))
	return out
}

type dpapiProtector struct {
	entropy []byte
}

func platformProtector(_ string) (Protector, error) {
	return dpapiProtector{entropy: dpapiEntropyMaterial}, nil
}

func (p dpapiProtector) Protect(plain []byte) ([]byte, error) {
	in := makeBlob(plain)
	ent := makeBlob(p.entropy)
	var out dataBlob
	r, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		uintptr(unsafe.Pointer(&ent)),
		0, // reserved
		0, // prompt struct
		cryptprotectUIForbid,
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(plain)
	runtime.KeepAlive(p.entropy)
	if r == 0 {
		return nil, fmt.Errorf("secrets: CryptProtectData failed: %v", callErr)
	}
	defer procLocalFreeSecrets.Call(uintptr(unsafe.Pointer(out.pbData)))
	return blobBytes(out), nil
}

func (p dpapiProtector) Unprotect(blob []byte) ([]byte, error) {
	in := makeBlob(blob)
	ent := makeBlob(p.entropy)
	var out dataBlob
	r, _, callErr := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description (out, unused)
		uintptr(unsafe.Pointer(&ent)),
		0,
		0,
		cryptprotectUIForbid,
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(blob)
	runtime.KeepAlive(p.entropy)
	if r == 0 {
		return nil, fmt.Errorf("secrets: CryptUnprotectData failed: %v", callErr)
	}
	defer procLocalFreeSecrets.Call(uintptr(unsafe.Pointer(out.pbData)))
	return blobBytes(out), nil
}
