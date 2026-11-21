//go:build !windows

package mihomo

import "os/exec"

// setProcAttrs is a no-op on non-Windows platforms.
func setProcAttrs(cmd *exec.Cmd) {}
