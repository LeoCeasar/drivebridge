package manager

import (
	"context"
	"errors"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
)

// ArchiveMount safely stops this local mount and hides its configuration. It
// retains every identity, path, accepted upload, conflict, and recovery record.
// The recovery stop policy still refuses open files, dirty VFS data, reservations,
// and failed unmounts; only already-durable work may remain queued offline.
func (m *Manager) ArchiveMount(ctx context.Context, id model.MountID) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	mount, err := m.ResolveMount(ctx, string(id))
	if err != nil {
		return err
	}
	if mount.Archived {
		return nil
	}
	disabled := false
	if _, err := m.desktopIntent(ctx, id, &disabled, false); err != nil {
		return err
	}
	if err := m.stopForRecovery(ctx, id, m.options.ReadyTimeout); err != nil {
		return errors.Join(ErrPending, err)
	}
	lock, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		return err
	}
	ok, err := lock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil {
		return errors.Join(ErrPending, err)
	}
	if !ok {
		return ErrPending
	}
	defer lock.Unlock()
	return m.updateConfig(ctx, func(c *config.Config) error {
		for i := range c.Mounts {
			if c.Mounts[i].ID == id {
				c.Mounts[i].Archived = true
				c.Mounts[i].Autostart = false
				return nil
			}
		}
		return ErrConfig
	})
}

// RestoreMount makes the preserved configuration visible again. Starting or
// enabling automatic uploads remains a separate explicit user action.
func (m *Manager) RestoreMount(ctx context.Context, id model.MountID) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	mount, err := m.ResolveMount(ctx, string(id))
	if err != nil {
		return err
	}
	if !mount.Archived {
		return nil
	}
	disabled := false
	if _, err := m.desktopIntent(ctx, id, &disabled, false); err != nil {
		return err
	}
	return m.updateConfig(ctx, func(c *config.Config) error {
		for i := range c.Mounts {
			if c.Mounts[i].ID == id {
				c.Mounts[i].Archived = false
				c.Mounts[i].Autostart = false
				return nil
			}
		}
		return ErrConfig
	})
}
