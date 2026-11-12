//go:build darwin

package safedelete

import (
	"fmt"
	"os"
	"path/filepath"
)

// removeTree on Darwin: the standard library lacks openat, so this
// fallback uses verified traversal — every directory's resolved
// path must equal its lexical path before descending (symlinks are
// removed as links, never followed). Darwin is a development
// platform only for DeBrowser (the product targets Windows, CI runs
// Linux + Windows); the anchored implementations live in
// safedelete_linux.go and safedelete_windows.go.
func removeTree(root, target string) error {
	return removeDirDarwin(target)
}

func removeDirDarwin(dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("safedelete: resolve %s: %w", dir, err)
	}
	if resolved != dir {
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("safedelete: remove link %s: %w", dir, err)
		}
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("safedelete: read dir %s: %w", dir, err)
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(child)
		if err != nil {
			return fmt.Errorf("safedelete: stat %s: %w", child, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(child); err != nil {
				return fmt.Errorf("safedelete: remove link %s: %w", child, err)
			}
			continue
		}
		if info.IsDir() {
			if err := removeDirDarwin(child); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(child); err != nil {
			return fmt.Errorf("safedelete: remove %s: %w", child, err)
		}
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("safedelete: rmdir %s: %w", dir, err)
	}
	return nil
}
