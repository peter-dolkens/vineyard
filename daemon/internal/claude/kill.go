package claude

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

// Terminate ends a Claude Code process: a polite SIGTERM first so it can flush its transcript, then
// SIGKILL if it is still around a few seconds later. Windows has no SIGTERM, so it is killed outright.
func Terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if !processAlive(pid) {
		return fmt.Errorf("process %d is not running", pid)
	}
	if runtime.GOOS == "windows" {
		return p.Kill()
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	go func() {
		time.Sleep(5 * time.Second)
		if processAlive(pid) {
			_ = p.Kill()
		}
	}()
	return nil
}

// WaitExit blocks until the process is gone or the timeout passes, reporting which. Terminate
// escalates to SIGKILL after 5 s, so a timeout a little above that catches everything.
func WaitExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}
