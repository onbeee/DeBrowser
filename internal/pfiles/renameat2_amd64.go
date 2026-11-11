//go:build linux && amd64

package pfiles

// SYS_RENAMEAT2 on linux/amd64 (not exported by stdlib syscall).
const sysRenameat2 = 316
