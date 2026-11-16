//go:build linux

package mount2

import (
	"context"
	"time"

	"github.com/rclone/rclone/vfs"
)

func waitManagedRenameReady(ctx context.Context, oldDir *vfs.Dir, oldName string, newDir *vfs.Dir, newName string) error {
	// Allow final FUSE Release to retire a closed writer, without closing a
	// surviving duplicated descriptor or retrying a namespace transaction.
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	return oldDir.WaitManagedRenameReady(ctx, oldName, newName, newDir)
}
