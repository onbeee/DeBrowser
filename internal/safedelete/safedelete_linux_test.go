//go:build linux

package safedelete

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// On Unix the traversal is anchored to open directory descriptors:
// a target reached THROUGH a symlinked intermediate component must
// be refused at the openat(O_NOFOLLOW) descent, before anything is
// deleted.
func TestRemoveTreeRefusesSymlinkedIntermediate(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "inner", "keep.txt"), "precious")
	if err := os.Symlink(outside, filepath.Join(root, "mid")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err := RemoveTree(root, filepath.Join(root, "mid", "inner"))
	if err == nil {
		t.Fatal("RemoveTree through a symlinked intermediate was allowed")
	}
	if _, err := os.Stat(filepath.Join(outside, "inner", "keep.txt")); err != nil {
		t.Errorf("outside sentinel was touched: %v", err)
	}
}

// The final component being a symlink: the link itself is removed,
// its target untouched.
func TestRemoveTreeRemovesFinalSymlinkItself(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := RemoveTree(root, link); err != nil {
		t.Fatalf("RemoveTree of a final symlink: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("link still exists (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("link target was touched: %v", err)
	}
}

// RemoveVerifiedAt must refuse when the slot's occupant is not the
// object whose identity was recorded: after a swap, the substitute
// is left untouched and an error is returned.
func TestRemoveVerifiedAtRefusesSwappedSlot(t *testing.T) {
	root := t.TempDir()
	slot := filepath.Join(root, "slot")
	writeFile(t, filepath.Join(slot, "data.txt"), "verified")

	rootFd, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rootFd.Close()

	fi, err := os.Lstat(slot)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	dev, ino := uint64(st.Dev), uint64(st.Ino)

	// Swap the verified directory for a substitute.
	parked := filepath.Join(root, "parked")
	if err := os.Rename(slot, parked); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(slot, "data.txt"), "substitute")

	err = RemoveVerifiedAt(int(rootFd.Fd()), "slot", dev, ino)
	if err == nil {
		t.Fatal("RemoveVerifiedAt deleted a swapped-in substitute; want refusal")
	}
	if _, err := os.Stat(filepath.Join(slot, "data.txt")); err != nil {
		t.Errorf("substitute was harmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parked, "data.txt")); err != nil {
		t.Errorf("verified object was harmed: %v", err)
	}

	// With the verified object back in place, the wipe proceeds.
	if err := os.RemoveAll(slot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parked, slot); err != nil {
		t.Fatal(err)
	}
	if err := RemoveVerifiedAt(int(rootFd.Fd()), "slot", dev, ino); err != nil {
		t.Fatalf("RemoveVerifiedAt on the verified object: %v", err)
	}
	if _, err := os.Lstat(slot); !os.IsNotExist(err) {
		t.Errorf("slot still exists after verified wipe (err=%v)", err)
	}
}
