//go:build !windows

package secrets

import (
	"fmt"
	"os"
	"path/filepath"
)

// Unix atomic replacement is rename(2): within one directory it
// atomically swaps the destination, and syncDir afterwards makes
// the directory entry itself durable.

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("secrets: rename: %w", err)
	}
	return nil
}

func syncDir(dir string) {
	d, err := os.Open(filepath.Clean(dir))
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
