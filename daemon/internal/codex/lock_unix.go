//go:build !windows

package codex

import (
	"os"
	"syscall"
)

// lockHeld reports whether another process holds the flock on path (Codex takes an exclusive
// flock on thread-writer-locks/<thread>.lock for as long as the thread is loaded). A shared lock is
// tried without blocking and released at once; a file nobody locks is not a live thread.
func lockHeld(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	return err == syscall.EWOULDBLOCK || err == syscall.EAGAIN
}
