//go:build windows

package codex

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
	procUnlockFile = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

// lockHeld reports whether another process holds the lock on path: Codex locks
// thread-writer-locks/<thread>.lock (LockFileEx, exclusive) while the thread is loaded. A shared
// lock is tried without waiting and released at once.
func lockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	var ol syscall.Overlapped
	h := syscall.Handle(f.Fd())
	r, _, e := procLockFileEx.Call(uintptr(h), uintptr(lockfileFailImmediately), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		_, _, _ = procUnlockFile.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
		return false
	}
	return e == errorLockViolation
}
