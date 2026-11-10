//go:build linux

package platform

import (
	"context"
	"os"
	"os/exec"
)

func DesktopAvailable() bool { return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" }
func openBrowser(ctx context.Context, u string) error {
	return exec.CommandContext(ctx, "xdg-open", u).Run()
}
func notify(ctx context.Context, title, message string) error {
	return exec.CommandContext(ctx, "notify-send", "--app-name=DriveBridge", "--", title, message).Run()
}
