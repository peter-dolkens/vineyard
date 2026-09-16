//go:build windows

package service

import (
	"os/exec"
	"syscall"
)

const detachedProcess = 0x00000008

// detach starts the child outside our console and process group so `schtasks /End` on the Vineyard
// task (which the installer itself issues) does not kill the installer.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess, HideWindow: true}
}
