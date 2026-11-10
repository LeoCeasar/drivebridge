package manager

import (
	"context"
	"errors"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

// WithQuiescence implements migrate.Quiescer. It excludes all worker starts and
// config writers, stops live mounts safely, and retains per-mount ownership
// locks through fn. Already offline durable queues remain intact for backup.
// fn must use its migration/config primitives, not recursively call manager
// config mutation methods or try to acquire another lifecycle lease.
func (m *Manager) WithQuiescence(ctx context.Context, fn func(context.Context) error) error {
	if fn == nil {
		return ErrConfig
	}
	release, err := m.lifecycleLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return err
	}
	for _, mount := range c.Mounts {
		lock, err := privateLock(m.runtimeLockPath(mount.ID))
		if err != nil {
			return err
		}
		ok, err := lock.TryLock()
		if err != nil {
			return err
		}
		if ok {
			_ = lock.Unlock()
			continue
		}
		if err = m.Stop(ctx, mount.ID, 60*time.Second); err != nil {
			return errors.Join(ErrPending, err)
		}
	}
	configLock, err := privateLock(m.options.ConfigFile + ".manager.lock")
	if err != nil {
		return err
	}
	ok, err := configLock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPending
	}
	defer configLock.Unlock()
	c, err = m.LoadConfig(ctx)
	if err != nil {
		return err
	}
	owners, err := m.owners(ctx)
	if err != nil {
		return err
	}
	for _, mount := range c.Mounts {
		spec, err := m.spec(ctx, mount.ID)
		if err != nil {
			return err
		}
		if err = m.validateOwnership(ctx, spec); err != nil {
			return err
		}
		owners = append(owners, ownershipFor(m.options.ConfigFile, spec))
	}
	seen := map[model.MountID]bool{}
	releases := []func(){}
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	for _, owner := range owners {
		if !seen[owner.Scope.MountID] {
			lock, err := privateLock(m.runtimeLockPath(owner.Scope.MountID))
			if err != nil {
				return err
			}
			ok, err := lock.TryLockContext(ctx, 5*time.Millisecond)
			if err != nil {
				return err
			}
			if !ok {
				return ErrPending
			}
			releases = append(releases, func() { _ = lock.Unlock() })
			seen[owner.Scope.MountID] = true
		}
		if pending, err := offlineCachePending(owner); err != nil || pending {
			return errors.Join(ErrPending, err)
		}
	}
	return fn(ctx)
}
