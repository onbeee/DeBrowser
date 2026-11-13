//go:build !windows

package secrets

import (
	"fmt"
	"os"
)

// Unix permission discipline (development platform): credential
// files are mode 0600 — owner read/write only, no group/other
// access at all. VerifyFileRestrictions fails on anything looser.

func RestrictFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secrets: chmod %s: %w", path, err)
	}
	return nil
}

func VerifyFileRestrictions(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("secrets: stat %s: %w", path, err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("secrets: %s has mode %o, group/other access must be empty", path, perm)
	}
	return nil
}
