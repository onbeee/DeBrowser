//go:build linux

package safedelete

// AT_REMOVEDIR for unlinkat(2) (not exported by the stdlib syscall
// package on Linux).
const atRemoveDir = 0x200
