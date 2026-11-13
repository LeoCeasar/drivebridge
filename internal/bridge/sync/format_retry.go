package syncer

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (e *Engine) retryCompatibilityConflict(ctx context.Context, j model.Job) (model.Job, error) {
	conflicts, err := e.store.Conflicts(ctx)
	if err != nil {
		return j, err
	}
	for _, c := range conflicts {
		if c.EntryID != j.EntryID || c.Generation != j.Generation {
			continue
		}
		var changed bool
		if c.Reason == "unsupported_content" {
			changed, err = e.store.RetryCompatibilityConflict(ctx, c)
		} else {
			if c.Base == "" || c.Local == "" || c.Remote == "" || (c.Draft != "" && c.Draft != c.Local) {
				continue
			}
			base, readErr := e.read(ctx, c.Base)
			if readErr != nil {
				return j, readErr
			}
			local, readErr := e.read(ctx, c.Local)
			if readErr != nil {
				return j, readErr
			}
			remote, readErr := e.read(ctx, c.Remote)
			if readErr != nil {
				return j, readErr
			}
			_, clean, mergeErr := history.MergeText(ctx, base, local, remote)
			if mergeErr != nil {
				return j, mergeErr
			}
			if !clean {
				continue
			}
			changed, err = e.store.RetryCleanTextConflict(ctx, c)
		}
		if err != nil {
			return j, err
		}
		if changed {
			return e.store.Job(ctx, j.ID)
		}
	}
	return j, nil
}
