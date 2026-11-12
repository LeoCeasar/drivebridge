package ui

import (
	"context"
	"errors"
	"os"
	"runtime"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

var ErrMountNotRunning = errors.New("mount is not running")

// hostGOOS is a variable so tests can verify per-platform wording.
var hostGOOS = runtime.GOOS

// FolderOpener is optional: fixture hosts that cannot show a real directory
// simply do not implement it and the UI hides the action.
type FolderOpener interface {
	OpenMountFolder(context.Context, model.MountID) error
}

// FolderOpenLabel names the host file manager. The page shows it verbatim.
func FolderOpenLabel(goos string) string {
	switch goos {
	case "windows":
		return "在资源管理器中打开"
	case "darwin":
		return "在 Finder 中打开"
	default:
		return "在文件管理器中打开"
	}
}

// OpenMountFolder opens only the configured mountpoint of a running, active
// mount. The browser sends a mount id and nothing else; no path is accepted.
func (b *ManagerBackend) OpenMountFolder(ctx context.Context, id model.MountID) error {
	c, err := b.manager.LoadConfig(ctx)
	if err != nil {
		return err
	}
	var mount model.Mount
	found := false
	for _, m := range c.Mounts {
		if m.ID == id {
			mount, found = m, true
		}
	}
	if !found {
		return os.ErrNotExist
	}
	if mount.Archived {
		return manager.ErrArchived
	}
	state, err := b.manager.Status(ctx, id)
	if err != nil {
		return err
	}
	if !state.Running {
		return ErrMountNotRunning
	}
	open := b.openFolder
	if open == nil {
		open = platform.OpenFolder
	}
	return open(mount.Mountpoint)
}

var _ FolderOpener = (*ManagerBackend)(nil)
