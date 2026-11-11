package ownedlogin

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

// The login helper owns its GUI; its private protocol never needs a console.
func configureHelperProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
