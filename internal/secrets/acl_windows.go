//go:build windows

package secrets

import (
	"fmt"
	"strings"
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
	advapi32                      = syscall.NewLazyDLL("advapi32.dll")
	procOpenProcessToken          = advapi32.NewProc("OpenProcessToken")
	procGetTokenInformation       = advapi32.NewProc("GetTokenInformation")
	procConvertSidToStringSidW    = advapi32.NewProc("ConvertSidToStringSidW")
	procConvertStringSDToSDW      = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescriptorDacl = advapi32.NewProc("GetSecurityDescriptorDacl")
	procSetNamedSecurityInfoW     = advapi32.NewProc("SetNamedSecurityInfoW")
	procGetNamedSecurityInfoW     = advapi32.NewProc("GetNamedSecurityInfoW")
	procConvertSDToStringSDW      = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procGetCurrentProcess         = kernel32ForSecrets.NewProc("GetCurrentProcess")
	procCloseHandleSecrets        = kernel32ForSecrets.NewProc("CloseHandle")
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

// VerifyFileRestrictions reads path's DACL back and proves it is
// exactly the protected three-principal descriptor RestrictFile
// applies: protected (P, no inheritance flags), full access for
// SYSTEM, Administrators and the current user, and NO other
// principal. Anything else is an error.
func VerifyFileRestrictions(path string) error {
	sid, err := currentUserSID()
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
	var strPtr *uint16
	r, _, callErr := procConvertSDToStringSDW.Call(sd, sddlRevision1,
		uintptr(daclSecurityInfo), uintptr(unsafe.Pointer(&strPtr)), 0)
	if r == 0 {
		return fmt.Errorf("secrets: ConvertSecurityDescriptorToString: %v", callErr)
	}
	defer procLocalFreeSecrets.Call(uintptr(unsafe.Pointer(strPtr)))
	got := utf16PtrToString(strPtr)

	if !strings.HasPrefix(got, "D:") {
		return fmt.Errorf("secrets: unexpected security descriptor %q", got)
	}
	rest := got[2:]
	open := strings.Index(rest, "(")
	if open < 0 {
		return fmt.Errorf("secrets: security descriptor has no ACEs: %q", got)
	}
	if flags := rest[:open]; flags != "P" {
		return fmt.Errorf("secrets: DACL is not protected-only (flags %q): %q", flags, got)
	}
	aces := rest[open:]
	for _, want := range []string{
		"(A;;FA;;;SY)", "(A;;FA;;;BA)", "(A;;FA;;;" + sid + ")",
	} {
		if !strings.Contains(aces, want) {
			return fmt.Errorf("secrets: DACL missing %s: %q", want, got)
		}
	}
	// Exactly three ACEs, all accounted for above: any additional
	// principal (Everyone, Users, ...) fails the count check.
	if n := strings.Count(aces, "(A;;"); n != 3 {
		return fmt.Errorf("secrets: DACL has %d allow ACEs, expected 3: %q", n, got)
	}
	if strings.Contains(aces, "(D;;") {
		return fmt.Errorf("secrets: DACL unexpectedly contains deny ACEs: %q", got)
	}
	return nil
}
