package syncer

import (
	"context"
	"time"

	"github.com/rclone/rclone/internal/bridge/doccache"
)

// Backfill old cached documents independently of uploads. Each pass is bounded
// by the existing local inventory, and failed downloads are retried next minute.
func (e *Engine) backfillDocImages(ctx context.Context) {
	retry := time.NewTicker(time.Minute)
	defer retry.Stop()
	for {
		e.backfillCachedDocImages(ctx)
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
		}
	}
}
func (e *Engine) backfillCachedDocImages(ctx context.Context) {
	entries, err := e.store.CachedDocuments(ctx)
	if err != nil {
		return
	}
	for _, en := range entries {
		if ctx.Err() != nil {
			return
		}
		if e.deps.ShouldRefresh != nil {
			ok, err := e.deps.ShouldRefresh(ctx, en.ID)
			if err != nil || !ok {
				continue
			}
		}
		sn, err := e.store.Snapshot(ctx, en.RemoteCheckpoint)
		if err != nil {
			continue
		}
		_ = doccache.Ensure(ctx, e.store, e.source, sn)
	}
}
