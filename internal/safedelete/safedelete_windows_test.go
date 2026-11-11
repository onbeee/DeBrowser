//go:build windows

package safedelete

import (
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

// After a file child has been opened and identity-verified, its
// PATH is moved out of the tree entirely. Deletion follows the
// verified object (the moved file is deleted through its handle);
// an unrelated outside sentinel is untouched and the wipe
// completes.
func TestHandleWipeChildMovedOutAfterOpen(t *testing.T) {
	root := t.TempDir()
	safe := t.TempDir()
	writeFile(t, filepath.Join(safe, "sentinel.txt"), "precious")
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "data.txt"), "user-data")
	writeFile(t, filepath.Join(target, "other.txt"), "x")

	moved := filepath.Join(safe, "moved.txt")
	HookAfterOpenChild = func(childDisplay string) {
		if filepath.Base(childDisplay) != "data.txt" {
			return
		}
		_ = os.Rename(filepath.Join(target, "data.txt"), moved)
	}
	defer func() { HookAfterOpenChild = nil }()

	h := openRootHandle(t, target)
	err := RemoveTreeFromHandle(h, target)
	_ = syscall.CloseHandle(h)
	HookAfterOpenChild = nil
	if err != nil {
		t.Fatalf("RemoveTreeFromHandle: %v", err)
	}
	if _, err := os.Stat(moved); !os.IsNotExist(err) {
		t.Errorf("the verified object was not deleted through its handle after being moved out (err=%v)", err)
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
