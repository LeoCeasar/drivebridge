//go:build darwin

package platform

import (
	"context"
	"os"
	"os/exec"
)

func DesktopAvailable() bool {
	if os.Getenv("SSH_TTY") != "" {
		return false
	}
	_, err := exec.LookPath("osascript")
	return err == nil
}
func openBrowser(ctx context.Context, u string) error {
	return exec.CommandContext(ctx, "open", u).Run()
}
func notify(ctx context.Context, title, message string) error {
	script := "on run argv\ndisplay notification (item 2 of argv) with title (item 1 of argv)\nend run"
	return exec.CommandContext(ctx, "osascript", "-e", script, "--", title, message).Run()
}
