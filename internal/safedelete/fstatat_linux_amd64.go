//go:build linux && amd64

package safedelete

// SYS_NEWFSTATAT on linux/amd64 (not exported by stdlib syscall).
const sysNewfstatat = 262
