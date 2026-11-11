package manager

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/vfs"
)

type cleanBodyStore interface {
	GC(context.Context, store.GCOptions) (store.GCResult, error)
	CleanCacheCandidates(context.Context) ([]store.CacheCandidate, error)
	EvictCleanEntry(context.Context, model.EntryID, uint64, ...time.Time) (bool, error)
}
type cleanBodyVFS interface {
	ManagedCacheBytes() int64
	TryExternalUpdate(context.Context, string, func() (fs.Object, error)) error
}
type bodyCacheSweepResult struct {
	EvictedEntries          int
	BytesBefore, BytesAfter int64
	OverBudget              bool
}

// sweepCleanBodies releases only reconstructible, idle clean bodies. Both durable
// Store blobs and fetched VFS data count toward the soft quota. The Store owns
// graph protection and prepared-blob grace; the VFS owns open/dirty reservations.
// publish must return the current local object without performing cloud I/O.
// Run this independently from metadata indexing and the upload engine: neither
// a slow directory listing nor cache collection should block the other workers.
func sweepCleanBodies(ctx context.Context, s cleanBodyStore, v cleanBodyVFS, publish func(context.Context, string) (fs.Object, error), maxAge time.Duration, budget int64, now time.Time) (result bodyCacheSweepResult, err error) {
	if err = ctx.Err(); err != nil {
		return
	}
	if s == nil || v == nil || publish == nil || maxAge < 0 || budget <= 0 {
		return result, errors.New("invalid body cache sweep options")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	measure := func() (int64, error) {
		if e := ctx.Err(); e != nil {
			return 0, e
		}
		gc, e := s.GC(ctx, store.GCOptions{Now: now, CleanBudgetBytes: budget})
		if e != nil {
			return 0, e
		}
		return gc.RetainedBytes + v.ManagedCacheBytes(), nil
	}
	total, err := measure()
	if err != nil {
		return result, err
	}
	result.BytesBefore, result.BytesAfter = total, total
	result.OverBudget = total > budget
	candidates, err := s.CleanCacheCandidates(ctx)
	if err != nil {
		return result, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.AccessedAt.Equal(b.AccessedAt) {
			return a.AccessedAt.Before(b.AccessedAt)
		}
		if a.Remote != b.Remote {
			return a.Remote < b.Remote
		}
		return a.EntryID < b.EntryID
	})
	for _, candidate := range candidates {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		expired := maxAge > 0 && !candidate.AccessedAt.IsZero() && now.Sub(candidate.AccessedAt) > maxAge
		if !expired && total <= budget {
			continue
		}
		// Bytes is deliberately not an eligibility filter: a shared body may free
		// nothing until the final eligible entry releases its reference.
		evicted := false
		var evictionErr error
		updateErr := v.TryExternalUpdate(ctx, candidate.Remote, func() (fs.Object, error) {
			evicted, evictionErr = s.EvictCleanEntry(ctx, candidate.EntryID, candidate.Generation, candidate.AccessedAt)
			if evictionErr != nil && !evicted {
				return nil, evictionErr
			}
			// Eviction may commit before cancellation or a later blob-unlink error.
			// Finish publishing that placeholder to the reserved live inode regardless.
			// A failed CAS also republishes the current object without discarding it.
			return publish(context.WithoutCancel(ctx), candidate.Remote)
		})
		if evicted {
			result.EvictedEntries++
		}
		if errors.Is(updateErr, vfs.ErrExternalUpdateBusy) && !evicted {
			continue
		}
		if updateErr != nil {
			return result, errors.Join(updateErr, evictionErr)
		}
		if err = ctx.Err(); err != nil {
			return result, errors.Join(err, evictionErr)
		}
		total, err = measure()
		if err != nil {
			return result, errors.Join(err, evictionErr)
		}
		result.BytesAfter = total
		result.OverBudget = total > budget
		if evictionErr != nil {
			return result, evictionErr
		}
	}
	return result, nil
}

var _ cleanBodyStore = (*store.Store)(nil)
var _ cleanBodyVFS = (*vfs.VFS)(nil)
