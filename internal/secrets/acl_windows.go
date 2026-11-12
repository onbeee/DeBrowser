//go:build windows

package secrets

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Windows file permission discipline for credential-bearing files
// (the vault, its backup, and — in Stage 4C — the Mihomo
// controller secret and the generated runtime config).
//
// RestrictFile replaces the file's DACL with a PROTECTED one
// granting full access to exactly three principals:
//
//   - the current Windows user (the vault's owner),
//   - Local SYSTEM (S-1-5-18),
//   - the local Administrators group (S-1-5-32-544).
//
// "Protected" (the P flag) severs ACL inheritance from the parent
// directory: the portable folder's own ACL — which the user may
// have set loosely, or which an E: drive may default wide open —
// can no longer widen access to the credential file. This does NOT
// rely on directory defaults; it is applied to the file itself and
// verified by reading the descriptor back (VerifyFileRestrictions).
// If either step fails, callers fail closed (see Store.Open/Put).
//
// The implementation uses advapi32 security-descriptor APIs via
// LazyDLL with an SDDL string, so the intended ACL is auditable in
// one line: D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;<user SID>).

var (
	advapi32                         = syscall.NewLazyDLL("advapi32.dll")
	procOpenProcessToken             = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation          = advapi32.NewProc("GetTokenInformation")
	procConvertSidToStringSidW       = advapi32.NewProc("ConvertSidToStringSidW")
	procConvertStringSDToSDW         = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescriptorDacl    = advapi32.NewProc("GetSecurityDescriptorDacl")
	procSetNamedSecurityInfoW        = advapi32.NewProc("SetNamedSecurityInfoW")
	procGetNamedSecurityInfoW        = advapi32.NewProc("GetNamedSecurityInfoW")
	procGetSecurityDescriptorControl = advapi32.NewProc("GetSecurityDescriptorControl")
	procGetAce                       = advapi32.NewProc("GetAce")
	procGetCurrentProcess            = kernel32ForSecrets.NewProc("GetCurrentProcess")
	procCloseHandleSecrets           = kernel32ForSecrets.NewProc("CloseHandle")
)

const (
	tokenQuery        = 0x0008
	tokenUser         = 1
	seFileObject      = 1
	daclSecurityInfo  = 0x00000004
	protectedDaclInfo = 0x80000000
	sddlRevision1     = 1
	sidSystem         = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"

	seDaclPresent        = 0x0004 // SE_DACL_PRESENT
	seDaclProtected      = 0x1000 // SE_DACL_PROTECTED
	accessAllowedAceType = 0      // ACCESS_ALLOWED_ACE_TYPE
	fileAllAccess        = 0x001F01FF
)

// currentUserSID returns the current process token's user SID in
// string form (for example "S-1-5-21-...-1001").
func currentUserSID() (string, error) {
	proc, _, _ := procGetCurrentProcess.Call()
	var token uintptr
	r, _, callErr := procOpenProcessToken.Call(proc, tokenQuery, uintptr(unsafe.Pointer(&token)))
	if r == 0 {
		return "", fmt.Errorf("secrets: OpenProcessToken: %v", callErr)
	}
	defer procCloseHandleSecrets.Call(token)

	var size uint32
	procGetTokenInformation.Call(token, tokenUser, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return "", fmt.Errorf("secrets: GetTokenInformation size query returned 0")
	}
	buf := make([]byte, size)
	r, _, callErr = procGetTokenInformation.Call(token, tokenUser,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(size), uintptr(unsafe.Pointer(&size)))
	if r == 0 {
		return "", fmt.Errorf("secrets: GetTokenInformation: %v", callErr)
	}
	// TOKEN_USER { SID_AND_ATTRIBUTES { PSID Sid; DWORD Attributes } }
	sidPtr := *(*uintptr)(unsafe.Pointer(&buf[0]))
	var strPtr *uint16
	r, _, callErr = procConvertSidToStringSidW.Call(sidPtr, uintptr(unsafe.Pointer(&strPtr)))
	if r == 0 {
		return "", fmt.Errorf("secrets: ConvertSidToStringSidW: %v", callErr)
	}
	defer procLocalFreeSecrets.Call(uintptr(unsafe.Pointer(strPtr)))
	return utf16PtrToString(strPtr), nil
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	units := unsafe.Slice(p, 4096)
	n := 0
	for n < len(units) && units[n] != 0 {
		n++
	}
	return syscall.UTF16ToString(units[:n])
}

// desiredSDDL builds the exact DACL this package enforces.
func desiredSDDL(userSID string) string {
	return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + userSID + ")"
}

// RestrictFile applies the protected three-principal DACL to path.
// Any failure is returned as an error; callers must not proceed on
// the assumption that permissions "are probably fine".
func RestrictFile(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	sddl, err := syscall.UTF16PtrFromString(desiredSDDL(sid))
	if err != nil {
		return fmt.Errorf("secrets: encode SDDL: %w", err)
	}
	var sd uintptr
	r, _, callErr := procConvertStringSDToSDW.Call(
		uintptr(unsafe.Pointer(sddl)), sddlRevision1,
		uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		return fmt.Errorf("secrets: ConvertStringSecurityDescriptor: %v", callErr)
	}
	defer procLocalFreeSecrets.Call(sd)

	var present, defaulted int32
	var dacl uintptr
	r, _, callErr = procGetSecurityDescriptorDacl.Call(sd,
		uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)))
	if r == 0 || present == 0 {
		return fmt.Errorf("secrets: GetSecurityDescriptorDacl: %v", callErr)
	}
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("secrets: encode path: %w", err)
	}
	rc, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), seFileObject,
		uintptr(daclSecurityInfo|protectedDaclInfo),
		0, 0, dacl, 0)
	if rc != 0 {
		return fmt.Errorf("secrets: SetNamedSecurityInfoW failed with code %d", rc)
	}
	return nil
}

// VerifyFileRestrictions reads path's DACL back and proves, at
// the binary ACE level, that it is exactly the descriptor
// RestrictFile applies:
//
//   - the DACL control has SE_DACL_PROTECTED set (inheritance from
//     the parent directory is severed — this is THE invariant);
//   - exactly three ACEs, all ACCESS_ALLOWED, each granting
//     FILE_ALL_ACCESS;
//   - the three trustees are precisely Local SYSTEM, the local
//     Administrators group and the current user (compared as
//     numeric SID strings, never SDDL aliases).
//
// Why not compare the SDDL string form: the string converter is
// lossy about presentation, not about content. A descriptor
// applied through SetNamedSecurityInfoW reads back with control
// flags "PAI" (SE_DACL_AUTO_INHERITED accompanies PROTECTED; it
// governs propagation to children and does not weaken the
// protected bit), and well-known trustees render as aliases (the
// runner's built-in Administrator account renders as "LA", not
// its numeric SID). Both are presentation details; the binary
// check below verifies the security content directly.
func VerifyFileRestrictions(path string) error {
	userSID, err := currentUserSID()
	if err != nil {
		return err
	}
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("secrets: encode path: %w", err)
	}
	var sd uintptr
	rc, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), seFileObject,
		uintptr(daclSecurityInfo), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&sd)))
	if rc != 0 {
		return fmt.Errorf("secrets: GetNamedSecurityInfoW failed with code %d", rc)
	}
	defer procLocalFreeSecrets.Call(sd)

	// SECURITY_DESCRIPTOR_CONTROL is a WORD; the revision is a DWORD.
	var control uint16
	var revision uint32
	r, _, callErr := procGetSecurityDescriptorControl.Call(sd,
		uintptr(unsafe.Pointer(&control)), uintptr(unsafe.Pointer(&revision)))
	if r == 0 {
		return fmt.Errorf("secrets: GetSecurityDescriptorControl: %v", callErr)
	}
	if control&seDaclPresent == 0 {
		return fmt.Errorf("secrets: security descriptor has no DACL")
	}
	if control&seDaclProtected == 0 {
		return fmt.Errorf("secrets: DACL is not protected (control %#x); directory inheritance still applies", control)
	}

	var present, defaulted int32
	var daclPtr unsafe.Pointer
	r, _, callErr = procGetSecurityDescriptorDacl.Call(sd,
		uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&daclPtr)),
		uintptr(unsafe.Pointer(&defaulted)))
	if r == 0 || present == 0 || daclPtr == nil {
		return fmt.Errorf("secrets: GetSecurityDescriptorDacl: %v", callErr)
	}
	// ACL header: revision(1) sbz(1) size(2) aceCount(2) sbz(2).
	aceCount := int(*(*uint16)(unsafe.Add(daclPtr, 4)))
	if aceCount != 3 {
		return fmt.Errorf("secrets: DACL has %d ACEs, expected exactly 3", aceCount)
	}
	want := map[string]bool{sidSystem: true, sidAdministrators: true, userSID: true}
	for i := 0; i < aceCount; i++ {
		var acePtr unsafe.Pointer
		r, _, callErr := procGetAce.Call(uintptr(daclPtr), uintptr(i), uintptr(unsafe.Pointer(&acePtr)))
		if r == 0 || acePtr == nil {
			return fmt.Errorf("secrets: GetAce %d: %v", i, callErr)
		}
		// ACCESS_ALLOWED_ACE: type(1) flags(1) size(2) mask(4) SID...
		aceType := *(*byte)(acePtr)
		if aceType != accessAllowedAceType {
			return fmt.Errorf("secrets: ACE %d has type %d, expected ACCESS_ALLOWED", i, aceType)
		}
		mask := *(*uint32)(unsafe.Add(acePtr, 4))
		if mask != fileAllAccess {
			return fmt.Errorf("secrets: ACE %d grants mask %#x, expected FILE_ALL_ACCESS", i, mask)
		}
		sidStr, err := sidPointerToString(uintptr(unsafe.Add(acePtr, 8)))
		if err != nil {
			return err
		}
		if !want[sidStr] {
			return fmt.Errorf("secrets: ACE %d grants unexpected principal %s", i, sidStr)
		}
		delete(want, sidStr)
	}
	if len(want) != 0 {
		return fmt.Errorf("secrets: DACL is missing expected principals %v", want)
	}
	return nil
}

// sidPointerToString converts a raw SID in memory to its numeric
// string form via ConvertSidToStringSidW.
func sidPointerToString(sid uintptr) (string, error) {
	var strPtr *uint16
	r, _, callErr := procConvertSidToStringSidW.Call(sid, uintptr(unsafe.Pointer(&strPtr)))
	if r == 0 {
		return "", fmt.Errorf("secrets: ConvertSidToStringSidW: %v", callErr)
	}
	defer procLocalFreeSecrets.Call(uintptr(unsafe.Pointer(strPtr)))
	return utf16PtrToString(strPtr), nil
}
