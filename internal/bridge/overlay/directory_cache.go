package overlay

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
)

type directoryRefresh struct {
	remote string
	done   chan struct{}
	err    error
}

// DirectoryCacheStatus describes metadata availability, not file-body downloads
// or an exhaustive inventory of every accessible cloud resource.
type DirectoryCacheStatus struct {
	Pending, Stale                    int
	LastRefreshed                     time.Time
	Recent                            []model.DirectoryRefreshStatus
	Known, Cached, Refreshing, Failed int
	Online                            *model.OnlineRecord
}

func (f *Fs) DirectoryStatus(ctx context.Context) (DirectoryCacheStatus, error) {
	var status DirectoryCacheStatus
	entries, err := f.store.DirectoryEntries(ctx)
	if err != nil {
		return status, err
	}
	status.Known = len(entries)
	now := time.Now()
	requests := map[model.EntryID]model.DirectoryRefreshStatus{}
	schedule := &f.directorySchedule
	schedule.mu.Lock()
	status.Pending = len(schedule.requests)
	for id, r := range schedule.requests {
		state := "queued"
		if r.running {
			state = "refreshing"
		} else if now.Before(r.readyAt) {
			state = "waiting"
		}
		requests[id] = model.DirectoryRefreshStatus{Remote: r.entry.Remote, State: state, NextAttemptAt: r.readyAt}
	}
	schedule.mu.Unlock()
	for _, e := range entries {
		metadata, err := f.store.Metadata(ctx, e.ID)
		if err != nil {
			return status, err
		}
		if metadata.DirectoryOnline != nil && !metadata.DirectoryOnline.Resolved {
			status.Failed++
			if status.Online == nil || (metadata.DirectoryOnline.Last != nil && (status.Online.Last == nil || metadata.DirectoryOnline.Last.At.After(status.Online.Last.At))) {
				status.Online = metadata.DirectoryOnline
			}
		}
		if metadata.DirectoryCachedAt.IsZero() || now.Sub(metadata.DirectoryCachedAt) >= directoryFreshFor {
			status.Stale++
		}
		if !metadata.DirectoryCachedAt.IsZero() {
			status.Cached++
			if metadata.DirectoryCachedAt.After(status.LastRefreshed) {
				status.LastRefreshed = metadata.DirectoryCachedAt
			}
		}
		recent, requested := requests[e.ID]
		if !requested {
			recent = model.DirectoryRefreshStatus{Remote: e.Remote, State: "cached"}
		}
		recent.CachedAt = metadata.DirectoryCachedAt
		if metadata.DirectoryOnline != nil && !metadata.DirectoryOnline.Resolved {
			recent.NextAttemptAt = metadata.DirectoryOnline.NotBefore
			if now.Before(recent.NextAttemptAt) {
				recent.State = "waiting"
			} else if !requested {
				recent.State = "failed"
			}
		}
		// Unobserved folders are coverage, not recent successful observations.
		if requested || !recent.CachedAt.IsZero() || recent.State == "waiting" || recent.State == "failed" {
			status.Recent = append(status.Recent, recent)
		}
	}
	sort.SliceStable(status.Recent, func(i, j int) bool {
		a, b := status.Recent[i], status.Recent[j]
		ap, bp := a.State != "cached", b.State != "cached"
		if ap != bp {
			return ap
		}
		if !a.CachedAt.Equal(b.CachedAt) {
			return a.CachedAt.After(b.CachedAt)
		}
		return a.Remote < b.Remote
	})
	if len(status.Recent) > 8 {
		status.Recent = status.Recent[:8]
	}
	f.refreshMu.Lock()
	status.Refreshing = len(f.refreshing)

	f.refreshMu.Unlock()
	return status, nil
}

// Existing committed rows are a usable local view when upgrading an older store
// that predates the explicit directory marker. Empty folders need one cold scan.
func (f *Fs) seedDirectoryCache(ctx context.Context) error { return f.store.SeedDirectoryCache(ctx) }

// RefreshCatalog projects the latest local discovery snapshot without network
// metadata requests. Callers can invalidate VFS categories immediately afterward.
func (f *Fs) RefreshCatalog(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.projectAccountCatalog(ctx)
}

func (f *Fs) projectAccountCatalog(ctx context.Context) error {
	catalog, ok := f.source.(accountCatalog)
	if !ok {
		return nil
	}
	dirs, sourceErr := catalog.accountDirectories(ctx) // Local snapshot only.
	f.listMu.Lock()
	defer f.listMu.Unlock()
	for _, d := range dirs {
		if err := f.observeItems(ctx, d.remote, d.token, d.items); err != nil {
			return errors.Join(sourceErr, err)
		}
	}
	return sourceErr
}

// RefreshDirectory explicitly refreshes this folder's metadata. Cached List
// calls remain local while this network request is pending or has failed.
func (f *Fs) RefreshDirectory(ctx context.Context, dir string) error {
	return f.refreshDirectory(ctx, dir, false)
}

func (f *Fs) refreshDirectory(ctx context.Context, dir string, coldOnly bool) error {
	return f.refreshDirectoryIdentity(ctx, dir, coldOnly, nil)
}
func (f *Fs) refreshDirectoryIdentity(ctx context.Context, dir string, coldOnly bool, expected *model.Entry) (err error) {
	ctx, release, err := f.beginDirectoryRead(ctx)
	if err != nil {
		return err
	}
	defer release()
	if allowed, err := f.SyncPathAllowed(ctx, dir); err != nil || !allowed {
		return err
	}
	if err := validPath(dir, true); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.listMu.Lock()
	parent, err := f.directory(ctx, dir)
	if err != nil {
		f.listMu.Unlock()
		return err
	}
	if expected != nil && (parent.ID != expected.ID || parent.Token != expected.Token || parent.Remote != expected.Remote) {
		f.listMu.Unlock()
		return model.ErrIdentityChanged
	}
	metadata, err := f.store.Metadata(ctx, parent.ID)
	if err != nil {
		f.listMu.Unlock()
		return err
	}
	if coldOnly && !metadata.DirectoryCachedAt.IsZero() {
		f.listMu.Unlock()
		return nil
	}
	f.refreshMu.Lock()
	if pending := f.refreshing[parent.ID]; pending != nil {
		f.refreshMu.Unlock()
		f.listMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-pending.done:
			return pending.err
		}
	}
	pending := &directoryRefresh{remote: dir, done: make(chan struct{})}
	// Capture before the network scan. A file created/bound while that scan is
	// in flight is not absent from an observation which predates its creation.
	before, readErr := f.store.Entries(ctx, dir)
	if readErr != nil {
		f.refreshMu.Unlock()
		f.listMu.Unlock()
		return readErr
	}
	f.refreshing[parent.ID] = pending
	f.refreshMu.Unlock()
	f.listMu.Unlock()
	defer func() {
		var failure *model.OnlineFailure
		if err != nil {
			captured := online.Capture(ctx, err, "directory_refresh", "local_commit", "not_dispatched", time.Now())
			failure = &captured
		}
		persistErr := f.store.RecordDirectoryOnline(context.WithoutCancel(ctx), parent, failure)
		err = errors.Join(err, persistErr)
		f.refreshMu.Lock()
		pending.err = err
		f.directoryFailures[parent.ID] = err != nil
		delete(f.refreshing, parent.ID)
		close(pending.done)
		f.refreshMu.Unlock()
	}()
	catalogErr := f.projectAccountCatalog(ctx)
	if parent.Token != "" {
		items, sourceErr := f.source.List(ctx, parent.Token)
		if sourceErr != nil {
			err = errors.Join(catalogErr, online.AtPhaseIfUnset(sourceErr, "directory_refresh", "list_parent"))
			return err
		}
		f.listMu.Lock()
		current, readErr := f.store.Entry(ctx, parent.ID)
		if readErr == nil && (current.Remote != parent.Remote || current.Token != parent.Token) {
			readErr = model.ErrIdentityChanged
		}
		if readErr == nil {
			readErr = f.observeItems(ctx, dir, parent.Token, items)
		}
		if readErr == nil {
			readErr = f.store.MarkDirectoryCached(ctx, parent)
		}
		f.listMu.Unlock()
		// VFS reconciliation takes native directory/cache locks. Never enter it
		// while holding listMu: a concurrent ReadDir can hold those locks and
		// be waiting for this same metadata lock.
		if readErr == nil {
			readErr = f.reconcileMissing(ctx, parent, before, items)
		}
		err = errors.Join(catalogErr, readErr)
		return err
	}
	err = errors.Join(catalogErr, f.store.MarkDirectoryCached(ctx, parent))
	return err
}

// Namespace admission waits only for metadata classification of its own parents,
// not an unrelated slow root. Warm directory reads never take this barrier.
func (f *Fs) lockNamespace(ctx context.Context, parents ...string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.listMu.Lock()
		f.refreshMu.Lock()
		var wait <-chan struct{}
		for _, pending := range f.refreshing {
			for _, remote := range parents {
				if pending.remote == remote {
					wait = pending.done
					break
				}
			}
			if wait != nil {
				break
			}
		}
		f.refreshMu.Unlock()
		if wait == nil {
			return nil
		}
		f.listMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

// RefreshDirectories force-refreshes cached or visited folders, continuing past
// individual errors. New roots are projected locally before any network work.
func (f *Fs) RefreshDirectories(ctx context.Context) error {
	var failures []error
	if err := f.projectAccountCatalog(ctx); err != nil {
		failures = append(failures, err)
	}
	entries, err := f.store.DirectoryEntries(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		metadata, err := f.store.Metadata(ctx, e.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		f.activityMu.Lock()
		visited := f.listed[e.ID]
		f.activityMu.Unlock()
		if metadata.DirectoryCachedAt.IsZero() && !visited {
			continue
		}
		if err := f.RefreshDirectory(ctx, e.Remote); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// WarmDirectories indexes metadata breadth-first, including unopened folders.
// Work is bounded per call and its queue resumes next call instead of repeatedly
// refreshing only the first batch. No file bodies are acquired.
func (f *Fs) WarmDirectories(ctx context.Context) error { return f.warmDirectoryBatch(ctx, 128) }

// WarmDirectoriesWithNotify emits committed metadata changes from the background
// indexer, outside filesystem callbacks and list/store locks.
func (f *Fs) WarmDirectoriesWithNotify(ctx context.Context, notify func(string)) error {
	return f.warmDirectoryBatch(ctx, 128, notify)
}
func (f *Fs) warmDirectoryBatch(ctx context.Context, limit int, notifications ...func(string)) error {
	f.warmMu.Lock()
	defer f.warmMu.Unlock()
	var failures []error
	if err := f.projectAccountCatalog(ctx); err != nil {
		failures = append(failures, err)
	}
	entries, err := f.store.DirectoryEntries(ctx)
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	if len(f.warmQueue) == 0 {
		clear(f.warmSeen)
		clear(f.warmQueued)
	}
	enqueue := func(e model.Entry) {
		if e.Kind == model.KindFolder && !f.warmSeen[e.ID] && !f.warmQueued[e.ID] {
			f.warmQueue = append(f.warmQueue, e.ID)
			f.warmQueued[e.ID] = true
		}
	}
	for _, e := range entries {
		enqueue(e)
	}
	for completed := 0; completed < limit; completed++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		// Priority completions can remain in the ordinary cursor. Skipping
		// them is not a scan opportunity and must not reset the burst budget.
		for len(f.warmQueue) > 0 && f.warmSeen[f.warmQueue[0]] {
			delete(f.warmQueued, f.warmQueue[0])
			f.warmQueue = f.warmQueue[1:]
		}
		request := f.takeDirectoryRequest(len(f.warmQueue) > 0)
		var id model.EntryID
		if request != nil {
			id = request.entry.ID
		} else {
			if len(f.warmQueue) == 0 {
				break
			}
			id = f.warmQueue[0]
			f.warmQueue = f.warmQueue[1:]
			delete(f.warmQueued, id)
			f.normalDirectoryTurn()
		}
		en, readErr := f.store.Entry(ctx, id)
		didRead := false
		if readErr == nil {
			allowed, policyErr := f.SyncPathAllowed(ctx, en.Remote)
			if policyErr != nil {
				readErr = policyErr
			} else if allowed {
				due, dueErr := f.directoryReadDue(ctx, id)
				if dueErr != nil {
					readErr = dueErr
				} else if due {
					expected := en
					if request != nil {
						expected = request.entry
					}
					readErr = f.refreshDirectoryIdentity(ctx, en.Remote, false, &expected)
					didRead = true
				}
			}
		}
		if request != nil {
			f.finishDirectoryRequest(ctx, request)
		}
		if ctx.Err() != nil {
			if request == nil {
				f.warmQueue = append([]model.EntryID{id}, f.warmQueue...)
				f.warmQueued[id] = true
			}
			return errors.Join(append(failures, ctx.Err())...)
		}
		f.warmSeen[id] = true
		if readErr == nil && didRead && len(notifications) > 0 && notifications[0] != nil {
			notifications[0](en.Remote)
		}
		if readErr != nil {
			failures = append(failures, readErr)
		}
		children, childrenErr := f.store.Entries(ctx, en.Remote)
		if childrenErr != nil {
			failures = append(failures, childrenErr)
			continue
		}
		for _, child := range children {
			enqueue(child)
		}
	}
	return errors.Join(failures...)
}
