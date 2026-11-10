package syncer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

// Run polls only explicitly active entries and directories already visited by the
// filesystem. Network mutations always go through the per-entry durable journal.
func (e *Engine) Run(ctx context.Context) error {
	jobs := time.NewTicker(time.Second)
	defer jobs.Stop()
	docs := time.NewTicker(e.opt.PollInterval)
	defer docs.Stop()
	dirs := time.NewTicker(e.opt.DirectoryInterval)
	defer dirs.Stop()
	for {
		if err := e.cycle(ctx); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-jobs.C:
		case <-docs.C:
			if e.deps.ActiveEntries != nil {
				for _, id := range e.deps.ActiveEntries() {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					_ = e.RefreshEntry(ctx, id)
				}
			}
		case <-dirs.C:
			if e.deps.RefreshDirectories != nil {
				_ = e.deps.RefreshDirectories(ctx)
			}
		}
	}
}

func (e *Engine) cycle(ctx context.Context) error {
	if err := e.check(ctx); err != nil {
		return err
	}
	// Namespace intent precedes dependent content, particularly new parent folders.
	namespaceErr := e.syncNamespace(ctx)
	jobs, err := e.store.Pending(ctx)
	if err != nil {
		return err
	}
	ids := map[model.EntryID]bool{}
	for _, j := range jobs {
		ids[j.EntryID] = true
	}
	publications, err := e.store.PendingPublications(ctx)
	if err != nil {
		return err
	}
	for _, p := range publications {
		ids[p.EntryID] = true
	}
	sem := make(chan struct{}, e.opt.Concurrency)
	var wg sync.WaitGroup
	for id := range ids {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func() { defer wg.Done(); defer func() { <-sem }(); _ = e.SyncEntry(ctx, id) }()
	}
	wg.Wait()
	if e.deps.OnConflict != nil {
		conflicts, err := e.store.Conflicts(ctx)
		if err != nil {
			return err
		}
		for _, conflict := range conflicts {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_ = e.deps.OnConflict(ctx, conflict)
		}
	}

	return namespaceErr
}
func (e *Engine) RefreshEntry(ctx context.Context, id model.EntryID) error {
	if err := e.check(ctx); err != nil {
		return err
	}
	unlock, err := e.entryLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	en, err := e.store.Entry(ctx, id)
	if err != nil {
		return err
	}
	if en.Token == "" || en.Visible == "" || en.FrozenBy != "" {
		return nil
	}
	pending, err := e.store.Pending(ctx)
	if err != nil {
		return err
	}
	for _, j := range pending {
		if j.EntryID == id {
			return nil
		}
	}
	var sn model.Snapshot
	if en.Kind == model.KindDocx {
		var fetchErr error
		sn, _, fetchErr = e.fetchDoc(ctx, en.Token)
		if fetchErr != nil {
			return fetchErr
		}
	} else if en.Kind == model.KindFile {
		sn, err = e.observeFile(ctx, en.Token)
		if err != nil {
			return err
		}
	} else {
		return nil
	}
	if en.RemoteCheckpoint != "" {
		prior, err := e.store.Snapshot(ctx, en.RemoteCheckpoint)
		if err != nil {
			return err
		}
		if en.Kind == model.KindDocx && sn.Revision == prior.Revision {
			return nil
		}
		if en.Kind == model.KindFile && sn.FileData == prior.FileData {
			return nil
		}
	}
	c, err := e.store.OfferRemote(ctx, id, en.Generation, sn, "")
	if err != nil {
		return err
	}
	if err = e.publisher.Publish(ctx, c); err != nil {
		return errors.Join(err, model.ErrVersionChanged)
	}
	return nil
}
