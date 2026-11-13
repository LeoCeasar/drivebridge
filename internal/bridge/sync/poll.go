package syncer

import (
	"context"
	"errors"
	"maps"
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
	cacheDone := make(chan struct{})
	go func() { defer close(cacheDone); e.backfillDocImages(refreshCtx) }()
	go func() {
		defer close(refreshDone)
		e.refreshActive(refreshCtx)
	}()
	defer func() {
		cancelRefresh()
		<-refreshDone
		<-cacheDone
	}()
	dispatcher := newBackgroundDispatcher(ctx, e)
	e.dispatchMu.Lock()
	e.dispatcher = dispatcher
	e.dispatchMu.Unlock()
	defer func() {
		dispatcher.close()
		e.dispatchMu.Lock()
		if e.dispatcher == dispatcher {
			e.dispatcher = nil
		}
		e.dispatchMu.Unlock()
	}()
	jobs := time.NewTicker(time.Second)
	defer jobs.Stop()
	dirs := time.NewTicker(e.opt.DirectoryInterval)
	defer dirs.Stop()
	for {
		if err := dispatcher.tick(); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case r := <-dispatcher.results:
			dispatcher.completed(r)
		case r := <-dispatcher.requests:
			dispatcher.accept(r)
		case <-jobs.C:
		case <-dirs.C:
			dispatcher.metadataWanted = true
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
	return e.cycleWithPolicy(ctx, false)
}
func (e *Engine) cycleWithPolicy(ctx context.Context, skipWriteBack bool) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.cycleGate:
	}
	defer func() { e.cycleGate <- struct{}{} }()
	if err := e.check(ctx); err != nil {
		return err
	}
	// A user is resolving conflicts: leave the shared account lock to them.
	if e.backgroundShouldYield() {
		return nil
	}
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
	cutoffs := map[model.EntryID]uint64{}
	for _, j := range jobs {
		ids[j.EntryID] = true
		if j.Generation > cutoffs[j.EntryID] {
			cutoffs[j.EntryID] = j.Generation
		}
	}
	publications, err := e.store.PendingPublications(ctx)
	if err != nil {
		return err
	}
	for _, p := range publications {
		ids[p.EntryID] = true
		if p.ExpectedGeneration > cutoffs[p.EntryID] {
			cutoffs[p.EntryID] = p.ExpectedGeneration
		}
	}
	// Capture durable content before namespace network calls. A save arriving
	// after this boundary cannot join a file's pass during the current cycle.
	namespaceErr := e.syncNamespace(ctx)
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
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			cutoff := cutoffs[id]
			_ = e.syncEntry(ctx, id, &cutoff, skipWriteBack)
		}()
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
		if en.Kind == model.KindDocx && sn.Revision == prior.Revision && sn.Markdown == prior.Markdown && sn.BlockMap == prior.BlockMap && maps.Equal(sn.DocMedia, prior.DocMedia) {
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
