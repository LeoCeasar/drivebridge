package syncer

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (e *Engine) retryCompatibilityConflict(ctx context.Context, j model.Job) (model.Job, error) {
	conflicts, err := e.store.Conflicts(ctx)
	if err != nil {
		return j, err
	}
	for _, c := range conflicts {
		if c.EntryID != j.EntryID || c.Generation != j.Generation || c.Reason != "unsupported_content" {
			continue
		}
		changed, err := e.store.RetryCompatibilityConflict(ctx, c)
		if err != nil {
			return j, err
		}
		if changed {
			return e.store.Job(ctx, j.ID)
		}
	}
	return j, nil
}
