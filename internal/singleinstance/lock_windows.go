//go:build windows

package singleinstance

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// Windows byte-range locking via LockFileEx, loaded from kernel32
// through the standard library only (no x/sys dependency). A
// one-byte exclusive lock at offset 4096 arbitrates the whole
// instance lock: the OS releases it automatically when the holding
// process dies — cleanly, by crash, or by force kill — so a stale
// lock can never survive its owner. The arbitration byte sits far
// past the informational PID content at the start of the file
// (see lockOffset): Windows byte-range locks are mandatory, so
// locking any of the leading bytes would also block other
// processes from reading the PID recorded there.

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
	procUnlockFile = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

// Win32 error codes reported when the byte range is held by another
// process (not exported by the standard library's syscall package).
const (
	errnoLockViolation    = syscall.Errno(33) // ERROR_LOCK_VIOLATION
	errnoSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
)

// overlapped mirrors the Win32 OVERLAPPED structure layout for the
// synchronous call (all zero: offset 0 via the Offset fields).
type overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       uintptr
}

// lockOffset is where the arbitration byte lives: far past the
// informational PID content at the start of the file. Windows byte
// range locks are MANDATORY — a lock covering byte 0 would also
// block other processes from READING the PID recorded there.
const lockOffset = 4096

// tryLock attempts a non-blocking exclusive lock of one byte at
// lockOffset in f. It returns errLocked when another process
// already holds it. Locking beyond end-of-file is explicitly
// allowed by LockFileEx.
func tryLock(f *os.File) error {
	var ol overlapped
	ol.Offset = lockOffset
	r1, _, callErr := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0, // reserved
		1, // lock one byte (low)
		0, // (high)
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		if errors.Is(callErr, errnoLockViolation) || errors.Is(callErr, errnoSharingViolation) {
			return errLocked
		}
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return errors.New("singleinstance: LockFileEx failed")
	}
	return nil
}

// unlock releases the byte-range lock. Deleting the lock file is
// deliberately NOT part of release: unlinking a locked file would
// let a newcomer create and lock a different inode while a third
// process still holds the old one.
func unlock(f *os.File) error {
	var ol overlapped
	ol.Offset = lockOffset
	r1, _, callErr := procUnlockFile.Call(
		f.Fd(),
		0, // reserved
		1, // unlock one byte (low)
		0, // (high)
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		if callErr != syscall.Errno(0) {
			return callErr
		}
		return errors.New("singleinstance: UnlockFileEx failed")
	}
	return nil
}
