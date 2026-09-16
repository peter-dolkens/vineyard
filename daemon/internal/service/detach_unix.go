//go:build !windows

package service

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session so that stopping this service (launchd kills the job's
// process group; a plain SIGTERM to us) does not take the installer down with it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
