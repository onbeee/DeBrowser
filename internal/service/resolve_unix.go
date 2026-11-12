//go:build !windows

package service

import "path/filepath"

// resolveFinalPath returns the fully resolved path of p with every
// symbolic-link component expanded. On Unix, filepath.EvalSymlinks
// resolves intermediate and final components alike.
func resolveFinalPath(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
