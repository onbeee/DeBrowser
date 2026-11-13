//go:build !windows

package service

import "os"

// Unix replacement is a same-filesystem rename, which atomically
// replaces the destination: a failure leaves an existing
// destination untouched and never leaves a partial file behind.
func platformReplaceFile(src, dst string) error {
	return os.Rename(src, dst)
}
