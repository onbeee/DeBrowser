//go:build linux

package safedelete

import (
	"os"
	"path/filepath"
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
