package safedelete

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTreeNormal(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "a.txt"), "a")
	writeFile(t, filepath.Join(target, "sub", "deep", "b.txt"), "b")
	writeFile(t, filepath.Join(target, "sub", "c.txt"), "c")
	other := filepath.Join(root, "other.txt")
	writeFile(t, other, "keep")

	if err := RemoveTree(root, target); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists (err=%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("sibling file removed: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("root removed: %v", err)
	}
}

func TestRemoveTreeRefusesNonChildTargets(t *testing.T) {
	root := t.TempDir()
	if err := RemoveTree(root, root); err == nil {
		t.Error("removing the root itself was allowed")
	}
	if err := RemoveTree(root, filepath.Join(root, "..", "elsewhere")); err == nil {
		t.Error("removing a path outside the root was allowed")
	}
}

func TestRemoveTreeNeverFollowsLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinelDir := filepath.Join(outside, "vault")
	writeFile(t, filepath.Join(sentinelDir, "keep.txt"), "precious")
	sentinelFile := filepath.Join(outside, "file.txt")
	writeFile(t, sentinelFile, "precious")

	target := filepath.Join(root, "prof")
	writeFile(t, filepath.Join(target, "real.txt"), "x")
	if err := os.Symlink(sentinelDir, filepath.Join(target, "dirlink")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(sentinelFile, filepath.Join(target, "filelink")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sub := filepath.Join(target, "sub")
	writeFile(t, filepath.Join(sub, "inner.txt"), "y")
	if err := os.Symlink(sentinelDir, filepath.Join(sub, "deeplink")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := RemoveTree(root, target); err != nil {
		t.Fatalf("RemoveTree with links inside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sentinelDir, "keep.txt")); err != nil {
		t.Errorf("outside dir sentinel was touched: %v", err)
	}
	if _, err := os.Stat(sentinelFile); err != nil {
		t.Errorf("outside file sentinel was touched: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target still exists (err=%v)", err)
	}
}

func TestRemoveTreeUnderComponentSwapRace(t *testing.T) {
	// While the tree is being deleted, another goroutine keeps
	// swapping the target entry between the real directory and a
	// symlink to an outside vault. Whatever the interleaving, the
	// vault must survive untouched — the deletion may succeed or
	// refuse, but it may never follow the swapped-in link.
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "keep.txt"), "precious")
	target := filepath.Join(root, "victim")
	realDir := filepath.Join(root, "victim-real")
	for i := 0; i < 40; i++ {
		writeFile(t, filepath.Join(realDir, "sub", strings.Repeat("f", 3)+string(rune('a'+i%26))+string(rune('0'+i/26))+".txt"), "x")
	}
	if err := os.Rename(realDir, target); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Swap target <-> symlink to outside, best effort; any
			// step may race the deleter and fail — that's fine.
			tmp := filepath.Join(root, "victim-parked")
			if err := os.Rename(target, tmp); err == nil {
				_ = os.Symlink(outside, target)
				time.Sleep(time.Millisecond)
				_ = os.Remove(target)
				_ = os.Rename(tmp, target)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_ = RemoveTree(root, target)
	close(stop)
	wg.Wait()
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("outside sentinel was touched during swap race: %v", err)
	}
}
