//go:build linux && arm64

package pfiles

// SYS_RENAMEAT2 on linux/arm64 (not exported by stdlib syscall).
const sysRenameat2 = 276
