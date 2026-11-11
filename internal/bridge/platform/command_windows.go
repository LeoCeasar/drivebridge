package platform

import (
	"context"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// BackgroundCommand prepares a short-lived console utility for a desktop-owned
// operation. Pipes and cancellation remain attached; this does not detach it or
// hide an application's own UI. Use the ordinary exec command for GUI programs.
func BackgroundCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd
}
