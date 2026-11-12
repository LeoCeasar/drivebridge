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
	// A grep can activate many bodies. Keep their observations off the upload
	// loop so one slow download cannot delay another entry's durable job.
	refreshCtx, cancelRefresh := context.WithCancel(ctx)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		e.refreshActive(refreshCtx)
	}()
	defer func() {
		cancelRefresh()
		<-refreshDone
	}()
	jobs := time.NewTicker(time.Second)
	defer jobs.Stop()
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
		case <-dirs.C:
			if e.deps.RefreshDirectories != nil {
				_ = e.deps.RefreshDirectories(ctx)
			}
		}
	}
}

// refreshActive uses one worker and finishes each finite snapshot before
// taking another. Coalesced ticker events cannot grow a queue or repeatedly
// restart a scan and starve its tail. RefreshEntry retains its per-entry lock,
// pending-write priority and background-pull policy checks.
func (e *Engine) refreshActive(ctx context.Context) {
	docs := time.NewTicker(e.opt.PollInterval)
	defer docs.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-docs.C:
			if e.deps.ActiveEntries == nil || e.backgroundShouldYield() {
				continue
			}
			for _, id := range e.deps.ActiveEntries() {
				if ctx.Err() != nil {
					return
				}
				_ = e.RefreshEntry(ctx, id)
			}
		}
	}
}

func (e *Engine) cycle(ctx context.Context) error {
	if err := e.check(ctx); err != nil {
		return err
	}
	// A user is resolving conflicts: leave the shared account lock to them.
	if e.backgroundShouldYield() {
		return nil
	}
	// Namespace intent precedes dependent content, particularly new parent folders.
	namespaceErr := e.syncNamespace(ctx)
	jobs, err := e.store.Pending(ctx)
	if err != nil {
		return err
	}
	pendingIDs := map[model.JobID]bool{}
	for _, j := range jobs {
		pendingIDs[j.ID] = true
	}
	e.retryMu.Lock()
	for id := range e.retries {
		if !pendingIDs[id] {
			delete(e.retries, id)
		}
	}
	e.retryMu.Unlock()
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
	// Background refresh policy never gates SyncEntry: local writes still need
	// upload readback, conflict checks, and durable backup observations.
	if e.deps.ShouldRefresh != nil {
		ok, err := e.deps.ShouldRefresh(ctx, id)
		if err != nil || !ok {
			return err
		}
	}
	var sn model.Snapshot
	if en.Kind == model.KindDocx {
		var fetchErr error
		sn, _, fetchErr = e.fetchDoc(ctx, en.Token)
		if fetchErr != nil {
			return e.deletionAfterReadFailure(ctx, en, fetchErr)
		}
	} else if en.Kind == model.KindFile {
		sn, err = e.observeFile(ctx, en.Token)
		if err != nil {
			return e.deletionAfterReadFailure(ctx, en, err)
		}
	} else {
		return nil
	}
	if en.RemoteCheckpoint != "" {
		prior, err := e.store.Snapshot(ctx, en.RemoteCheckpoint)
		if err != nil {
			return err
		}
		if en.Kind == model.KindDocx && sn.Revision == prior.Revision && sn.Markdown == prior.Markdown && sn.BlockMap == prior.BlockMap {
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
