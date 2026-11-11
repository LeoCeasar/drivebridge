package manager

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (m *Manager) SetBackgroundPullPolicy(ctx context.Context, id model.MountID, extensions []string) error {
	rules, err := config.EffectiveBackgroundPullExcludedExtensions(model.Mount{BackgroundPullExcludedExtensions: extensions})
	if err != nil {
		return ErrConfig
	}
	return m.admittedConfigUpdate(ctx, func(c *config.Config) error {
		for i := range c.Mounts {
			if c.Mounts[i].ID == id {
				c.Mounts[i].BackgroundPullExcludedExtensions = rules
				return nil
			}
		}
		return ErrConfig
	})
}

// Read current policy rather than the worker's startup snapshot: editing only
// background refresh rules must not stop active files or pending uploads.
func backgroundPullAllowed(ctx context.Context, load func(context.Context) (config.Config, error), lookup func(context.Context, model.EntryID) (model.Entry, error), mountID model.MountID, entryID model.EntryID) (bool, error) {
	c, err := load(ctx)
	if err != nil {
		return false, err
	}
	for _, mount := range c.Mounts {
		if mount.ID == mountID {
			rules, err := config.EffectiveBackgroundPullExcludedExtensions(mount)
			if err != nil {
				return false, err
			}
			entry, err := lookup(ctx, entryID)
			if err != nil {
				return false, err
			}
			return !config.BackgroundPullExcluded(entry.Remote, rules), nil
		}
	}
	return false, ErrConfig
}
