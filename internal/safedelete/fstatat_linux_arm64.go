//go:build linux && arm64

package safedelete

// SYS_NEWFSTATAT on linux/arm64 (not exported by stdlib syscall).
const sysNewfstatat = 79
