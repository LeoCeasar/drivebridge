package larkcli

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// CLI requests use pipes instead of a terminal, including when launched from the
// tray's GUI process. Keep the child attached for normal cancellation/reaping.
func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}
