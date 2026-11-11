package overlay

import (
	"context"
	"errors"

	"github.com/rclone/rclone/internal/bridge/model"
)

type directoryRefresh struct {
	remote string
	done   chan struct{}
	err    error
}

// DirectoryCacheStatus describes metadata availability, not file-body downloads
// or an exhaustive inventory of every accessible cloud resource.
type DirectoryCacheStatus struct{ Known, Cached, Refreshing, Failed int }

func (f *Fs) DirectoryStatus(ctx context.Context) (DirectoryCacheStatus, error) {
	var status DirectoryCacheStatus
	entries, err := f.store.DirectoryEntries(ctx)
	if err != nil {
		return status, err
	}
	status.Known = len(entries)
	known := make(map[model.EntryID]bool, len(entries))
	for _, e := range entries {
		known[e.ID] = true
		metadata, err := f.store.Metadata(ctx, e.ID)
		if err != nil {
			return status, err
		}
		if !metadata.DirectoryCachedAt.IsZero() {
			status.Cached++
		}
	}
	f.refreshMu.Lock()
	status.Refreshing = len(f.refreshing)
	for id, failed := range f.directoryFailures {
		if failed && known[id] {
			status.Failed++
		}
	}
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
	f.refreshing[parent.ID] = pending
	f.refreshMu.Unlock()
	f.listMu.Unlock()
	defer func() {
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
			err = errors.Join(catalogErr, sourceErr)
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
	for completed := 0; completed < limit && len(f.warmQueue) > 0; completed++ {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		id := f.warmQueue[0]
		f.warmQueue = f.warmQueue[1:]
		delete(f.warmQueued, id)
		e, err := f.store.Entry(ctx, id)
		if err == nil {
			err = f.RefreshDirectory(ctx, e.Remote)
		}
		if ctx.Err() != nil {
			f.warmQueue = append([]model.EntryID{id}, f.warmQueue...)
			f.warmQueued[id] = true
			return errors.Join(append(failures, ctx.Err())...)
		}
		f.warmSeen[id] = true
		if err == nil && len(notifications) > 0 && notifications[0] != nil {
			notifications[0](e.Remote)
		}
		if err != nil {
			failures = append(failures, err)
		}
		children, readErr := f.store.Entries(ctx, e.Remote)
		if readErr != nil {
			failures = append(failures, readErr)
			continue
		}
		for _, child := range children {
			enqueue(child)
		}
	}
	return errors.Join(failures...)
}
