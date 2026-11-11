//go:build !windows

package platform

import (
	"context"
	"os/exec"
)

// BackgroundCommand leaves ordinary Unix process and terminal behavior intact.
func BackgroundCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
