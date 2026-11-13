//go:build windows

package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// Windows-only tests: real DPAPI, the real ACL implementation and
// MoveFileEx-based replacement. They run in GitHub Actions on the
// Windows runner (this package's Linux runs use the AES-GCM
// development protector and cannot exercise these paths).

func TestDPAPIRoundTripRealVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.vault")
	s, rec, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if rec.Unrecoverable {
		t.Fatalf("fresh vault unrecoverable: %+v", rec)
	}
	ref, err := s.Put([]byte("proxy-password-123"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// The file on disk must not contain the plaintext anywhere.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "proxy-password-123") {
		t.Fatal("plaintext credential found in vault file")
	}
	s2, _, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s2.Get(ref)
	if err != nil || string(got) != "proxy-password-123" {
		t.Fatalf("get = %q, %v", got, err)
	}
}

func TestDPAPIEntropyMismatchCannotDecrypt(t *testing.T) {
	// A blob protected under different optional entropy must not
	// decrypt — the same failure class as a vault moved to another
	// Windows user or machine, which the store reports as
	// ErrCorrupt rather than returning wrong data.
	def, err := platformProtector("")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := def.Protect([]byte("topsecret"))
	if err != nil {
		t.Fatalf("protect: %v", err)
	}
	other := dpapiProtector{entropy: []byte("DeBrowser/secrets/other")}
	if _, err := other.Unprotect(blob); err == nil {
		t.Fatal("blob decrypted under wrong entropy")
	}
	if plain, err := def.Unprotect(blob); err != nil || string(plain) != "topsecret" {
		t.Fatalf("correct-entropy decrypt = %q, %v", plain, err)
	}
}

func TestACLRestrictAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acl-probe.bin")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestrictFile(path); err != nil {
		t.Fatalf("restrict: %v", err)
	}
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("verify after restrict: %v", err)
	}
	// Restricting twice must be a stable no-op success.
	if err := RestrictFile(path); err != nil {
		t.Fatalf("second restrict: %v", err)
	}
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("verify after second restrict: %v", err)
	}
}

// ---- ACL verifier negative tests ---------------------------------
//
// The verifier must accept exactly one DACL shape (see
// VerifyFileRestrictions) and reject everything else. Each test
// below constructs one specific deviation and asserts rejection —
// the positive counterpart is TestACLRestrictAndVerify. These
// tests guard against future "fixes" that would loosen the
// verifier to make a red CI green.

// applySDDLForTest replaces path's DACL with the one described by
// an SDDL string ("D:..." without header flags), optionally
// marking it protected, using the same advapi32 primitives as the
// production code.
func applySDDLForTest(t *testing.T, path, sddl string, protected bool) {
	t.Helper()
	sddlW, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	var sd uintptr
	r, _, callErr := procConvertStringSDToSDW.Call(
		uintptr(unsafe.Pointer(sddlW)), sddlRevision1,
		uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		t.Fatalf("ConvertStringSecurityDescriptor(%q): %v", sddl, callErr)
	}
	defer procLocalFreeSecrets.Call(sd)
	var present, defaulted int32
	var dacl uintptr
	r, _, callErr = procGetSecurityDescriptorDacl.Call(sd,
		uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)))
	if r == 0 {
		t.Fatalf("GetSecurityDescriptorDacl: %v", callErr)
	}
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	info := uintptr(daclSecurityInfo)
	if protected {
		info |= uintptr(protectedDaclInfo)
	}
	rc, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), seFileObject, info, 0, 0, dacl, 0)
	if rc != 0 {
		t.Fatalf("SetNamedSecurityInfoW(%q) failed with code %d", sddl, rc)
	}
}

func TestVerifyRejectsDefaultFileDACL(t *testing.T) {
	// A newly created file carries the directory's inherited,
	// unprotected DACL: verification must fail.
	path := filepath.Join(t.TempDir(), "plain.bin")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFileRestrictions(path); err == nil {
		t.Fatal("default inherited DACL passed verification")
	}
}

func TestVerifyRejectsSDDLDeviations(t *testing.T) {
	userSID, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	good := "(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + userSID + ")"
	cases := []struct {
		name      string
		sddl      string
		protected bool
	}{
		{"unprotected correct DACL", "D:" + good, false},
		{"extra ACE for Users", "D:" + good + "(A;;FA;;;BU)", true},
		{"extra ACE with unknown SID", "D:" + good + "(A;;FA;;;S-1-5-21-1000000000-2000000000-3000000000-1001)", true},
		{"missing current user ACE", "D:(A;;FA;;;SY)(A;;FA;;;BA)", true},
		{"deny ACE present", "D:(A;;FA;;;SY)(A;;FA;;;BA)(D;;FA;;;WD)", true},
		{"user ACE with read-only mask", "D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;" + userSID + ")", true},
		{"Users instead of current user", "D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;BU)", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe.bin")
			if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
				t.Fatal(err)
			}
			applySDDLForTest(t, path, tc.sddl, tc.protected)
			err := VerifyFileRestrictions(path)
			if err == nil {
				t.Fatalf("deviation %q passed verification", tc.name)
			}
			t.Logf("rejected as expected: %v", err)
		})
	}
	// Control: the same SDDL applied WITH protection must pass, so
	// the rejections above are attributable to the deviations.
	path := filepath.Join(t.TempDir(), "probe.bin")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	applySDDLForTest(t, path, "D:"+good, true)
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("control DACL rejected: %v", err)
	}
}

var (
	procInitializeAcl          = advapi32.NewProc("InitializeAcl")
	procAddAccessAllowedAceEx  = advapi32.NewProc("AddAccessAllowedAceEx")
	procConvertStringSidToSidW = advapi32.NewProc("ConvertStringSidToSidW")
)

// buildACLWithUserAceFlags constructs a DACL granting
// FILE_ALL_ACCESS to SYSTEM, Administrators and the current user,
// with the given AceFlags on the current user's ACE, and applies
// it (protected) to path. SDDL cannot express arbitrary AceFlags,
// so this uses InitializeAcl/AddAccessAllowedAceEx directly.
func buildACLWithUserAceFlags(t *testing.T, path string, userAceFlags uintptr) {
	t.Helper()
	userSID, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sidFor := func(s string) uintptr {
		w, err := syscall.UTF16PtrFromString(s)
		if err != nil {
			t.Fatal(err)
		}
		var sid uintptr
		r, _, callErr := procConvertStringSidToSidW.Call(
			uintptr(unsafe.Pointer(w)), uintptr(unsafe.Pointer(&sid)))
		if r == 0 {
			t.Fatalf("ConvertStringSidToSidW(%s): %v", s, callErr)
		}
		t.Cleanup(func() { procLocalFreeSecrets.Call(sid) })
		return sid
	}
	acl := make([]byte, 1024)
	r, _, callErr := procInitializeAcl.Call(
		uintptr(unsafe.Pointer(&acl[0])), uintptr(len(acl)), 2) // ACL_REVISION
	if r == 0 {
		t.Fatalf("InitializeAcl: %v", callErr)
	}
	add := func(flags, sid uintptr) {
		r, _, callErr := procAddAccessAllowedAceEx.Call(
			uintptr(unsafe.Pointer(&acl[0])), 2, flags, fileAllAccess, sid)
		if r == 0 {
			t.Fatalf("AddAccessAllowedAceEx: %v", callErr)
		}
	}
	add(0, sidFor(sidSystem))
	add(0, sidFor(sidAdministrators))
	add(userAceFlags, sidFor(userSID))
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), seFileObject,
		uintptr(daclSecurityInfo|protectedDaclInfo),
		0, 0, uintptr(unsafe.Pointer(&acl[0])), 0)
	if rc != 0 {
		t.Fatalf("SetNamedSecurityInfoW failed with code %d", rc)
	}
}

func TestVerifyRejectsAceFlags(t *testing.T) {
	cases := []struct {
		name  string
		flags uintptr
	}{
		{"inherit only", 0x8},             // INHERIT_ONLY_ACE: ACE would not apply to the file itself
		{"object+container inherit", 0x3}, // OI|CI: meaningless for a file, never set by RestrictFile
		{"inherited ace marker", 0x10},    // INHERITED_ACE: claims the ACE came from a parent
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe.bin")
			if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
				t.Fatal(err)
			}
			buildACLWithUserAceFlags(t, path, tc.flags)
			if err := VerifyFileRestrictions(path); err == nil {
				t.Fatalf("ACE flags %#x passed verification", tc.flags)
			} else {
				t.Logf("rejected as expected: %v", err)
			}
		})
	}
	// Control: identical construction with zero flags must pass.
	path := filepath.Join(t.TempDir(), "probe.bin")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	buildACLWithUserAceFlags(t, path, 0)
	if err := VerifyFileRestrictions(path); err != nil {
		t.Fatalf("zero-flag control DACL rejected: %v", err)
	}
}

func TestVerifyRejectsNullDACL(t *testing.T) {
	// A NULL DACL means "no protection at all" (everyone full
	// access). Apply one via the SDDL NO_ACCESS_CONTROL form and
	// assert rejection.
	path := filepath.Join(t.TempDir(), "probe.bin")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	sddlW, err := syscall.UTF16PtrFromString("D:NO_ACCESS_CONTROL")
	if err != nil {
		t.Fatal(err)
	}
	var sd uintptr
	r, _, callErr := procConvertStringSDToSDW.Call(
		uintptr(unsafe.Pointer(sddlW)), sddlRevision1,
		uintptr(unsafe.Pointer(&sd)), 0)
	if r == 0 {
		t.Fatalf("ConvertStringSecurityDescriptor(NO_ACCESS_CONTROL): %v", callErr)
	}
	defer procLocalFreeSecrets.Call(sd)
	var present, defaulted int32
	var dacl uintptr
	r, _, callErr = procGetSecurityDescriptorDacl.Call(sd,
		uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)))
	if r == 0 {
		t.Fatalf("GetSecurityDescriptorDacl: %v", callErr)
	}
	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathW)), seFileObject,
		uintptr(daclSecurityInfo), 0, 0, dacl, 0)
	if rc != 0 {
		t.Fatalf("SetNamedSecurityInfoW(NULL DACL) failed with code %d", rc)
	}
	if err := VerifyFileRestrictions(path); err == nil {
		t.Fatal("NULL DACL passed verification")
	}
}

func TestReplaceFileOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.bin")
	for i, want := range []string{"one", "two-two", "three-three-three"} {
		src := filepath.Join(dir, "src.bin")
		if err := os.WriteFile(src, []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := replaceFile(src, dst); err != nil {
			t.Fatalf("replace %d: %v", i, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != want {
			t.Fatalf("after replace %d: %q, %v", i, got, err)
		}
	}
}

func TestVaultPutFailsInMissingDirectory(t *testing.T) {
	// Opening a not-yet-created vault is fine, but persisting
	// into a nonexistent directory must fail loudly at Put time —
	// never report a credential as stored when it is not.
	bad := filepath.Join(t.TempDir(), "no-such-dir", "secrets.vault")
	s, _, err := Open(bad)
	if err != nil {
		t.Fatalf("open fresh vault: %v", err)
	}
	if _, err := s.Put([]byte("x")); err == nil {
		t.Fatal("put into missing directory succeeded")
	} else if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("unexpected error class: %v", err)
	}
}
