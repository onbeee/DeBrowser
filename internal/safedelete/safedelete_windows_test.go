//go:build windows

package safedelete

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable: %v (%s)", err, out)
	}
}

func openRootHandle(t *testing.T, path string) syscall.Handle {
	t.Helper()
	h, err := openObject(path)
	if err != nil {
		t.Fatalf("open root %s: %v", path, err)
	}
	return h
}

// The handle-anchored engine deletes a normal tree completely,
// including nested directories, and removes inner links as links
// only.
func TestHandleWipeNormalTree(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "a.txt"), "a")
	writeFile(t, filepath.Join(target, "sub", "deep", "b.txt"), "b")
	if err := os.Symlink(outside, filepath.Join(target, "dirlink")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	makeJunction(t, filepath.Join(target, "sub", "junc"), outside)

	h := openRootHandle(t, target)
	if err := RemoveTreeFromHandle(h, target); err != nil {
		t.Fatalf("RemoveTreeFromHandle: %v", err)
	}
	_ = syscall.CloseHandle(h)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists after handle wipe (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
}

// Headline scenario: the tree's PATH is taken over after its
// handle was opened — the root directory is renamed away and a
// junction to an outside directory (holding an impostor tree and a
// sentinel) is planted at the old path. The wipe must still delete
// exactly the object behind the handle and nothing else.
func TestHandleWipeRootPathSwappedBeforeWipe(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "sub", "Preferences"), "user-data")
	writeFile(t, filepath.Join(outside, "prof", "sentinel.txt"), "impostor")
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")

	h := openRootHandle(t, target)

	// Swap the namespace: real tree parked, junction at its path.
	parked := filepath.Join(root, "prof-parked")
	if err := os.Rename(target, parked); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, target, outside)

	if err := RemoveTreeFromHandle(h, target); err != nil {
		t.Fatalf("RemoveTreeFromHandle after root swap: %v", err)
	}
	_ = syscall.CloseHandle(h)
	if _, err := os.Stat(parked); !os.IsNotExist(err) {
		t.Errorf("the ORIGINAL tree (parked) was not the one wiped (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "prof", "sentinel.txt")); err != nil {
		t.Errorf("impostor tree inside the junction target was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
}

// After enumeration, a child directory is replaced by a junction
// to an outside vault. The relative open now lands on the junction
// object, whose file ID differs from the enumerated one: the wipe
// must abort (fail closed), delete neither the junction nor
// anything in the vault, and leave the real child untouched.
func TestHandleWipeChildSwappedToJunctionAfterEnumerate(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "sub", "Preferences"), "user-data")

	parked := filepath.Join(root, "sub-parked")
	HookAfterEnumerate = func(dirDisplay string) {
		if dirDisplay != target {
			return
		}
		if err := os.Rename(filepath.Join(target, "sub"), parked); err != nil {
			return
		}
		cmd := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(target, "sub"), outside)
		_ = cmd.Run()
	}
	defer func() { HookAfterEnumerate = nil }()

	h := openRootHandle(t, target)
	err := RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	HookAfterEnumerate = nil
	if err == nil {
		t.Fatal("wipe through a swapped-in junction succeeded; want fail-closed identity error")
	}
	if !strings.Contains(err.Error(), "identity changed") {
		t.Errorf("error should report the identity change, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parked, "Preferences")); err != nil {
		t.Errorf("the real child directory was harmed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "sub")); err != nil {
		t.Errorf("the planted junction itself should be left in place on abort: %v", err)
	}
}

// Same swap, one level deeper: the replacement happens while the
// wipe is recursing (enumeration of an intermediate directory),
// proving the identity chain holds at every depth, not just at
// the root.
func TestHandleWipeIntermediateSwappedDuringRecursion(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(root, "prof")
	inner := filepath.Join(target, "a")
	writeFile(t, filepath.Join(inner, "b", "deep.txt"), "user-data")

	parked := filepath.Join(root, "b-parked")
	HookAfterEnumerate = func(dirDisplay string) {
		if dirDisplay != inner {
			return
		}
		if err := os.Rename(filepath.Join(inner, "b"), parked); err != nil {
			return
		}
		cmd := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(inner, "b"), outside)
		_ = cmd.Run()
	}
	defer func() { HookAfterEnumerate = nil }()

	h := openRootHandle(t, target)
	err := RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	HookAfterEnumerate = nil
	if err == nil {
		t.Fatal("wipe succeeded despite an intermediate swap during recursion; want fail-closed error")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parked, "deep.txt")); err != nil {
		t.Errorf("the real intermediate directory was harmed: %v", err)
	}
}

// After a file child has been opened and identity-verified, an
// external rename tries to move it out of the tree. Child handles
// are opened WITHOUT FILE_SHARE_DELETE, so the move must be
// BLOCKED by the sharing mode (it cannot succeed behind the
// wipe's back): the file is then deleted at its in-tree location,
// nothing appears outside, and the wipe completes. (This replaces
// the earlier expectation that deletion followed an already-moved
// object — deleting data that has left the profile boundary is
// exactly what must not happen.)
func TestHandleWipeChildCannotBeMovedOutAfterOpen(t *testing.T) {
	root := t.TempDir()
	safe := t.TempDir()
	writeFile(t, filepath.Join(safe, "sentinel.txt"), "precious")
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "data.txt"), "user-data")
	writeFile(t, filepath.Join(target, "other.txt"), "x")

	moved := filepath.Join(safe, "moved.txt")
	var renameErr error
	HookAfterOpenChild = func(childDisplay string) {
		if filepath.Base(childDisplay) != "data.txt" {
			return
		}
		renameErr = os.Rename(filepath.Join(target, "data.txt"), moved)
	}
	defer func() { HookAfterOpenChild = nil }()

	h := openRootHandle(t, target)
	err := RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	HookAfterOpenChild = nil
	if err != nil {
		t.Fatalf("RemoveTreeFromHandle: %v", err)
	}
	if renameErr == nil {
		t.Error("the move-out rename SUCCEEDED while the wipe held the child open; want a sharing violation (FILE_SHARE_DELETE must not be granted)")
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Errorf("the file appeared outside the tree (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(target, "data.txt")); !os.IsNotExist(err) {
		t.Errorf("data.txt was not deleted in place (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(safe, "sentinel.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists (err=%v)", err)
	}
}

// A locked file inside the tree makes the wipe fail with an error
// naming the tree's recorded path; the data is retained (no
// silent loss) and a retry after unlocking succeeds.
func TestHandleWipeFailureReportsPathAndRetainsData(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	locked := filepath.Join(target, "locked.txt")
	writeFile(t, locked, "user-data")

	lockHandle, err := syscall.CreateFile(
		mustUTF16(t, locked), syscall.GENERIC_READ, 0, nil,
		syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("lock file: %v", err)
	}

	h := openRootHandle(t, target)
	err = RemoveTreeFromHandle(h, target)
	if err == nil {
		_ = syscall.CloseHandle(lockHandle)
		_ = syscall.CloseHandle(h)
		t.Fatal("wipe of a tree with a locked file succeeded; want a reported failure")
	}
	if !strings.Contains(err.Error(), target) {
		t.Errorf("failure must name the residual tree path %s, got: %v", target, err)
	}
	if _, serr := os.Stat(locked); serr != nil {
		t.Errorf("data was silently lost despite the failed wipe: %v", serr)
	}
	_ = syscall.CloseHandle(lockHandle)
	if err := RemoveTreeFromHandle(h, target); err != nil {
		t.Fatalf("retry after unlock: %v", err)
	}
	_ = syscall.CloseHandle(h)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists after successful retry (err=%v)", err)
	}
}

func mustUTF16(t *testing.T, s string) *uint16 {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A file child replaced by a NEW file (same name) between
// enumeration and open: the 128-bit identity differs, so the wipe
// must abort and the substitute must survive untouched.
func TestHandleWipeFileSwappedAfterEnumerate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "victim.txt"), "original")
	writeFile(t, filepath.Join(target, "other.txt"), "x")

	HookAfterEnumerate = func(dirDisplay string) {
		if dirDisplay != target {
			return
		}
		if err := os.Remove(filepath.Join(target, "victim.txt")); err != nil {
			return
		}
		_ = os.WriteFile(filepath.Join(target, "victim.txt"), []byte("substitute"), 0o644)
	}
	defer func() { HookAfterEnumerate = nil }()

	h := openRootHandle(t, target)
	err := RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	HookAfterEnumerate = nil
	if err == nil {
		t.Fatal("wipe deleted a swapped-in substitute file; want fail-closed identity error")
	}
	if !strings.Contains(err.Error(), "identity changed") {
		t.Errorf("error should report the identity change, got: %v", err)
	}
	data, rerr := os.ReadFile(filepath.Join(target, "victim.txt"))
	if rerr != nil || string(data) != "substitute" {
		t.Errorf("substitute file was harmed (data=%q err=%v)", data, rerr)
	}
}

// The strongest substitution form: after enumeration the child
// is DELETED outright and a brand-new object is created under the
// same name. Microsoft documents that file systems are free to
// reuse file IDs after deletion, so the identity guarantee would
// be void if the recreated object actually inherited the
// enumerated 128-bit ID — on NTFS it does not (a replacement gets
// a different file reference), and this test pins that behavior:
// the wipe must fail closed and the recreated file must survive
// byte-for-byte. (TestHandleWipeFileSwappedAfterEnumerate covers
// the same window via a remove+write in one step; this test
// isolates the delete-then-recreate sequence by name.)
func TestHandleWipeFileRecreatedAfterEnumerate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "victim.txt"), "original")
	victim := filepath.Join(target, "victim.txt")
	fired := false
	HookAfterEnumerate = func(dirDisplay string) {
		if dirDisplay != target || fired {
			return
		}
		fired = true
		if err := os.Remove(victim); err != nil {
			t.Errorf("hook remove: %v", err)
			return
		}
		if err := os.WriteFile(victim, []byte("recreated"), 0o644); err != nil {
			t.Errorf("hook recreate: %v", err)
		}
	}
	defer func() { HookAfterEnumerate = nil }()

	h := openRootHandle(t, target)
	err := RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	if err == nil {
		t.Fatal("wipe succeeded after the enumerated file was deleted and recreated; want an identity failure")
	}
	if !strings.Contains(err.Error(), "object identity changed during wipe") {
		t.Fatalf("wipe failed for an unexpected reason: %v", err)
	}
	data, rerr := os.ReadFile(victim)
	if rerr != nil || string(data) != "recreated" {
		t.Errorf("recreated file was harmed: data=%q err=%v", data, rerr)
	}
}

// An empty directory wipes cleanly (the enumeration ends with
// ERROR_NO_MORE_FILES on the first, restart, call).
func TestHandleWipeEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "empty")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	h := openRootHandle(t, target)
	if err := RemoveTreeFromHandle(h, target); err != nil {
		t.Fatalf("RemoveTreeFromHandle on empty dir: %v", err)
	}
	_ = syscall.CloseHandle(h)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("empty dir still exists (err=%v)", err)
	}
}

// A directory with far more entries than one 64 KiB buffer holds
// (3000 small files ≈ 350 KB of records) forces multi-batch
// enumeration across the restart/continue classes; every entry
// must be deleted.
func TestHandleWipeManyFilesMultiBatch(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		if err := os.WriteFile(filepath.Join(target, "file-"+itoa(i)+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := openRootHandle(t, target)
	if err := RemoveTreeFromHandle(h, target); err != nil {
		t.Fatalf("RemoveTreeFromHandle on 3000 files: %v", err)
	}
	_ = syscall.CloseHandle(h)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists after multi-batch wipe (err=%v)", err)
	}
}

// Unicode names and long names survive the UTF-16 round trip
// through the extended directory records.
func TestHandleWipeUnicodeAndLongNames(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "文件-üñïçødé-データ.txt"), "u")
	writeFile(t, filepath.Join(target, strings.Repeat("长", 100)+".txt"), "long")
	writeFile(t, filepath.Join(target, "子目录", "内部.txt"), "nested")

	h := openRootHandle(t, target)
	if err := RemoveTreeFromHandle(h, target); err != nil {
		t.Fatalf("RemoveTreeFromHandle: %v", err)
	}
	_ = syscall.CloseHandle(h)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists (err=%v)", err)
	}
}

// Without FILE_LIST_DIRECTORY on the directory handle, the
// extended enumeration is impossible: the wipe must fail closed
// (error naming the tree) and delete nothing — it must never fall
// back to treating the directory as empty.
func TestHandleWipeWithoutListAccessFailsClosed(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "keep.txt"), "precious")

	p, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, 0,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, 0x02000000, 0) // BACKUP_SEMANTICS, access 0
	if err != nil {
		t.Fatalf("open no-access handle: %v", err)
	}
	err = RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	if err == nil {
		t.Fatal("wipe without enumeration access succeeded; want fail-closed error")
	}
	if _, serr := os.Stat(filepath.Join(target, "keep.txt")); serr != nil {
		t.Errorf("data was deleted despite the failed wipe: %v", serr)
	}
}

// Repeated wipes of equivalent trees behave identically (the
// engine keeps no state between runs).
func TestHandleWipeRepeatedRunsStable(t *testing.T) {
	for i := 0; i < 3; i++ {
		root := t.TempDir()
		target := filepath.Join(root, "prof")
		writeFile(t, filepath.Join(target, "sub", "a.txt"), "a")
		h := openRootHandle(t, target)
		if err := RemoveTreeFromHandle(h, target); err != nil {
			_ = syscall.CloseHandle(h)
			t.Fatalf("run %d: %v", i, err)
		}
		_ = syscall.CloseHandle(h)
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("run %d: target still exists (err=%v)", i, err)
		}
	}
}

// The FILE_ID_EXTD_DIR_INFO parser reads names, attributes and
// the full 128-bit file ID at the documented offsets, follows
// NextEntryOffset, and rejects malformed records instead of
// silently skipping them.
func TestParseExtdDirEntries(t *testing.T) {
	mkEntry := func(buf []byte, off int, next uint32, attrs uint32, idSeed byte, name string) {
		binary.LittleEndian.PutUint32(buf[off:], next)
		binary.LittleEndian.PutUint32(buf[off+56:], attrs)
		u16 := utf16Encode(name)
		binary.LittleEndian.PutUint32(buf[off+60:], uint32(2*len(u16)))
		for i := 0; i < 16; i++ {
			buf[off+72+i] = idSeed + byte(i)
		}
		for i, c := range u16 {
			binary.LittleEndian.PutUint16(buf[off+88+2*i:], c)
		}
	}
	buf := make([]byte, 512)
	mkEntry(buf, 0, 96, 0x10, 0xA0, "ab")     // 88 + 4 name bytes, padded to 96
	mkEntry(buf, 96, 0, 0x20, 0xB0, "cdefgh") // terminal entry
	var out []dirEntry
	walked, err := parseExtdDirEntries(buf, &out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if walked != 2 || len(out) != 2 {
		t.Fatalf("walked=%d entries=%d, want 2/2", walked, len(out))
	}
	if out[0].name != "ab" || out[0].attrs != 0x10 || out[0].fileID[0] != 0xA0 || out[0].fileID[15] != 0xAF {
		t.Errorf("entry 0 wrong: %+v", out[0])
	}
	if out[1].name != "cdefgh" || out[1].attrs != 0x20 || out[1].fileID[0] != 0xB0 {
		t.Errorf("entry 1 wrong: %+v", out[1])
	}

	// Malformed records must error, never be skipped.
	bad := make([]byte, 512)
	mkEntry(bad, 0, 40, 0x10, 0xA0, "ab") // NextEntryOffset below the header size
	if _, err := parseExtdDirEntries(bad, &[]dirEntry{}); err == nil {
		t.Error("NextEntryOffset below header size was accepted")
	}
	bad2 := make([]byte, 512)
	mkEntry(bad2, 0, 0, 0x10, 0xA0, "ab")
	binary.LittleEndian.PutUint32(bad2[60:], 9999) // name length overruns the buffer
	if _, err := parseExtdDirEntries(bad2, &[]dirEntry{}); err == nil {
		t.Error("over-long file name was accepted")
	}
	bad3 := make([]byte, 512)
	mkEntry(bad3, 0, 0, 0x10, 0xA0, "ab")
	binary.LittleEndian.PutUint32(bad3[60:], 3) // odd name length
	if _, err := parseExtdDirEntries(bad3, &[]dirEntry{}); err == nil {
		t.Error("odd file name length was accepted")
	}
	// NextEntryOffset that cuts through the record's own file
	// name: "abcdefgh" occupies 88+16=104 bytes of record, but the
	// next record is placed at 96, INSIDE that name. The second
	// record is itself well-formed, so only the record-extent
	// check (NextEntryOffset >= header + FileNameLength) can
	// reject this buffer — without it the walk would happily read
	// overlapping records.
	bad4 := make([]byte, 512)
	mkEntry(bad4, 0, 96, 0x10, 0xA0, "abcdefgh")
	mkEntry(bad4, 96, 0, 0x10, 0xB0, "zz")
	if _, err := parseExtdDirEntries(bad4, &[]dirEntry{}); err == nil {
		t.Error("NextEntryOffset smaller than header+file name was accepted (records overlap)")
	}
	// A file name length at the uint32 boundary: 88 + 0xFFFFFFFE
	// must not wrap to a small number in the bounds arithmetic.
	bad5 := make([]byte, 512)
	mkEntry(bad5, 0, 0, 0x10, 0xA0, "ab")
	binary.LittleEndian.PutUint32(bad5[60:], 0xFFFFFFFE)
	if _, err := parseExtdDirEntries(bad5, &[]dirEntry{}); err == nil {
		t.Error("file name length 0xFFFFFFFE was accepted (bounds arithmetic overflow)")
	}
	// Misaligned NextEntryOffset.
	bad6 := make([]byte, 512)
	mkEntry(bad6, 0, 97, 0x10, 0xA0, "ab")
	if _, err := parseExtdDirEntries(bad6, &[]dirEntry{}); err == nil {
		t.Error("misaligned NextEntryOffset was accepted")
	}
	// A well-formed first record followed by a malformed second
	// record must fail the buffer as a whole. The caller
	// (enumerateFromHandle) discards partial results on error, so
	// no entry from a malformed buffer is ever acted on — a parse
	// error can therefore never lead to a deletion.
	bad7 := make([]byte, 512)
	mkEntry(bad7, 0, 96, 0x10, 0xA0, "ab")
	mkEntry(bad7, 96, 40, 0x10, 0xB0, "cd")
	if _, err := parseExtdDirEntries(bad7, &[]dirEntry{}); err == nil {
		t.Error("malformed second record was accepted")
	}
	// Valid single terminal record.
	one := make([]byte, 512)
	mkEntry(one, 0, 0, 0x20, 0xC0, "solo")
	var oneOut []dirEntry
	walkedOne, err := parseExtdDirEntries(one, &oneOut)
	if err != nil || walkedOne != 1 || len(oneOut) != 1 || oneOut[0].name != "solo" {
		t.Errorf("valid single record: walked=%d entries=%v err=%v", walkedOne, oneOut, err)
	}
}

func utf16Encode(s string) []uint16 {
	out, _ := syscall.UTF16FromString(s)
	return out[:len(out)-1]
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
