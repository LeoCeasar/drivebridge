package manager

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
)

// RefreshDirectory queues a read on an already-running mount. It neither starts
// an offline worker nor retries uploads or clears a service waiting bound.
func (m *Manager) RefreshDirectory(ctx context.Context, id model.MountID, remote string) error {
	if !model.ValidDirectoryRemote(remote) {
		return ErrConfig
	}
	return m.call(ctx, id, "/refresh-directory", model.DirectoryRefreshRequest{Remote: remote}, nil)
}
