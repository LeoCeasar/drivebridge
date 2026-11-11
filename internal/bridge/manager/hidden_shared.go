package manager

import (
	"context"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (s discoverySettings) hiddenSharedEnabled() bool {
	return s.SyncHiddenShared == nil || *s.SyncHiddenShared
}

// SetHiddenSharedSync changes only this account's local preference. Discovery
// snapshots, local bodies and durable jobs remain untouched. An unavailable IPC
// invalidation cannot undo the already-saved preference: workers read it live.
func (m *Manager) SetHiddenSharedSync(ctx context.Context, id model.ProfileID, enabled bool) (DiscoveryView, error) {
	release, err := m.startLease(ctx)
	if err != nil {
		return DiscoveryView{}, err
	}
	defer release()
	if _, err = m.ResolveProfile(ctx, string(id)); err != nil {
		return DiscoveryView{}, err
	}
	unlock, err := m.lockDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	defer unlock()
	profile, err := m.ResolveProfile(ctx, string(id))
	if err != nil {
		return DiscoveryView{}, err
	}
	settings, snapshot, err := m.readDiscovery(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if settings.Binding.ProfileID == "" {
		return DiscoveryView{}, discovery.ErrNeedsLogin
	}
	if settings.Binding.ProfileID != string(id) || settings.Binding.IdentityHash != profile.IdentityHash || snapshot.Binding != settings.Binding {
		return DiscoveryView{}, model.ErrIdentityChanged
	}
	settings.SyncHiddenShared = &enabled
	root, err := m.discoveryDir(ctx, id)
	if err != nil {
		return DiscoveryView{}, err
	}
	if err = atomicJSON(filepath.Join(root, "settings.json"), settings); err != nil {
		return DiscoveryView{}, ErrConfig
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return DiscoveryView{}, err
	}
	// Local authenticated IPC only: do not request credentials, start a worker,
	// enable startup intent or create a replacement for an archived account disk.
	for _, remote := range c.Remotes {
		if remote.ProfileID != id || !remote.AccountRoot {
			continue
		}
		for _, mount := range c.Mounts {
			if mount.RemoteID != remote.ID || mount.Archived {
				continue
			}
			notify, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = m.call(notify, mount.ID, "/refresh-directories", struct{}{}, nil)
			cancel()
		}
	}
	return m.discoveryView(ctx, profile)
}
