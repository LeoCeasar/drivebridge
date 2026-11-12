package vfscache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/lib/ranges"
)

// The operations are per-cache dependencies, allowing storage failures to be
// returned at Flush/Sync rather than being hidden in an asynchronous close.
type managedIOOps struct {
	syncFile func(*os.File) error
	rename   func(string, string) error
}

const managedTempPrefix = ".rclone-managed-meta-"

// Called with Item.mu held. A failed replace leaves the previous valid checkpoint.
func (item *Item) saveManaged() error {
	target := item.c.toOSPathMeta(item.name)
	dir := filepath.Dir(target)
	f, e := os.CreateTemp(dir, managedTempPrefix)
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	if e = json.NewEncoder(f).Encode(item.info); e == nil {
		e = item.c.managedIO.syncFile(f)
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = item.c.managedIO.rename(temp, target); e != nil {
		return e
	}
	return syncParents(dir)
}
func (item *Item) lockMutation() func() {
	if !item.c.managed.Load() {
		return func() {}
	}
	item.snapshotMu.Lock()
	return item.snapshotMu.Unlock
}

// FlushManaged persists and stages one complete immutable byte snapshot. Missing
// ranges are downloaded before snapshot exclusion, so downloader writers never
// deadlock waiting for a lock held by their own waiter.
func (item *Item) FlushManaged(ctx context.Context, storeFn StoreFn) (fs.Object, error) {
	if !item.c.managed.Load() {
		return nil, errors.New("managed cache is disabled")
	}
	item.preAccess()
	defer item.postAccess()
	for {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		item.mu.Lock()
		if !item.info.Dirty {
			o := item.o
			item.mu.Unlock()
			return o, nil
		}
		if item.fd == nil {
			item.mu.Unlock()
			return nil, errors.New("managed flush requires open cache item")
		}
		e := item._ensure(0, item.info.Size)
		item.mu.Unlock()
		if e != nil {
			return nil, e
		}
		item.snapshotMu.Lock()
		item.mu.Lock()
		if !item.info.Rs.Present(ranges.Range{Pos: 0, Size: item.info.Size}) {
			item.mu.Unlock()
			item.snapshotMu.Unlock()
			continue
		}
		o, e := item.flushManagedLocked(ctx, storeFn)
		item.mu.Unlock()
		item.snapshotMu.Unlock()
		return o, e
	}
}

// Both snapshotMu and mu are held. snapshotMu remains held across local Stage;
// no user mutation or downloader overwrite can produce a hybrid input stream.
func (item *Item) flushManagedLocked(ctx context.Context, storeFn StoreFn) (fs.Object, error) {
	if !item.info.Dirty {
		return item.o, nil
	}
	seq := item.info.MutationSeq
	if e := item.c.managedIO.syncFile(item.fd); e != nil {
		return nil, e
	}
	if e := syncParents(filepath.Dir(item.c.toOSPath(item.name))); e != nil {
		return nil, e
	}
	previous := item.info.DurableSeq
	item.info.DurableSeq = seq
	if e := item.saveManaged(); e != nil {
		item.info.DurableSeq = previous
		return nil, e
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	in, e := os.Open(item.c.toOSPath(item.name))
	if e != nil {
		return nil, e
	}
	src := object.NewStaticObjectInfo(item.name, item.info.ModTime, item.info.Size, true, nil, item.c.fremote)
	o := item.o
	item.mu.Unlock()
	if o == nil {
		o, e = item.c.fremote.Put(ctx, in, src)
	} else {
		e = o.Update(ctx, in, src)
	}
	e = errors.Join(e, in.Close())
	item.mu.Lock()
	if e != nil {
		return nil, e
	}
	if o == nil || o.Size() != item.info.Size {
		return nil, fmt.Errorf("managed Stage returned inconsistent snapshot size")
	}
	item.o = o
	item._updateFingerprint()
	item.info.StagedSeq = seq
	item.info.Dirty = false
	if e = item.saveManaged(); e != nil {
		item.info.Dirty = true
		return nil, e
	}
	if storeFn != nil {
		item.mu.Unlock()
		storeFn(o)
		item.mu.Lock()
	}
	return o, nil
}

// FlushForManagedRename stages dirty data while the overlay namespace is frozen.
// It never schedules generic writeback or requires a cloud operation.
func (item *Item) FlushForManagedRename(ctx context.Context, fn StoreFn) (out fs.Object, err error) {
	item.mu.Lock()
	needOpen := item.fd == nil
	dirty := item.info.Dirty
	o := item.o
	item.mu.Unlock()
	if !dirty {
		return o, nil
	}
	if needOpen {
		if err = item.Open(o); err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, item.closeCache(nil)) }()
	}
	return item.FlushManaged(ctx, fn)
}

// FlushManagedPath does not create cache entries merely to check existence.
func (c *Cache) FlushManagedPath(ctx context.Context, name string, fn StoreFn) (fs.Object, error) {
	c.mu.Lock()
	item := c.item[clean(name)]
	c.mu.Unlock()
	if item == nil {
		return nil, nil
	}
	return item.FlushForManagedRename(ctx, fn)
}

// New cache directories may themselves have been created during this save.
func syncParents(dir string) error {
	for {
		if e := platform.SyncDir(dir); e != nil {
			return e
		}
		next := filepath.Dir(dir)
		if next == dir {
			return nil
		}
		dir = next
	}
}
