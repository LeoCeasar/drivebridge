//go:build darwin && amd64

package mount2

import (
	"context"

	"github.com/rclone/rclone/vfs"
)

func waitManagedRenameReady(ctx context.Context, oldDir *vfs.Dir, oldName string, newDir *vfs.Dir, newName string) error {
	return nil
}
