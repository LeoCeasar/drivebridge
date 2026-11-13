package vfscache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/ranges"
)

func (c *Cache) wakeManagedWorker() {
	if c.managedWake != nil {
		select {
		case c.managedWake <- struct{}{}:
		default:
		}
	}
}

// StopManagedArchives joins the worker before its Store is closed or files are
// replaced by an upgrade. Accepted, unfinished checkpoints stay on disk.
func (c *Cache) StopManagedArchives() {
	if c.managedCancel != nil {
		c.managedCancel()
		<-c.managedDone
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range c.item {
		item.closeManagedJournal()
	}
}

func (c *Cache) managedJournalWorker(ctx context.Context) {
	defer close(c.managedDone)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.managedWake:
		case <-tick.C:
		}
		c.mu.Lock()
		items := make([]*Item, 0, len(c.item))
		for _, i := range c.item {
			items = append(items, i)
		}
		c.mu.Unlock()
		for _, i := range items {
			if ctx.Err() != nil {
				return
			}
			c.archiveMu.RLock()
			err := i.stageManagedJournal(ctx, nil)
			c.archiveMu.RUnlock()
			if err != nil && ctx.Err() == nil {
				fs.Errorf(i.GetName(), "local save is preserved; background archive will retry: %v", err)
			}
		}
	}
}

// stageMu serializes archive/namespace drains. Foreground saves never take it.
// Per-checkpoint readers own immutable extent maps and file handles, so the
// whole-file import can run concurrently with later writes and checkpoints.
func (item *Item) stageManagedJournal(ctx context.Context, fn StoreFn) (err error) {
	item.stageMu.Lock()
	defer item.stageMu.Unlock()
	item.mu.Lock()
	if item.journal == nil {
		item.managedStageError = nil
		item.mu.Unlock()
		return nil
	}
	item.managedStaging = true
	item.mu.Unlock()
	defer func() { item.mu.Lock(); item.managedStaging = false; item.managedStageError = err; item.mu.Unlock() }()
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		item.mu.Lock()
		j := item.journal
		if j == nil {
			item.mu.Unlock()
			return nil
		}
		var cp *journalCheckpoint
		for _, v := range j.checkpoints {
			if v.info.MutationSeq > item.info.StagedSeq {
				copy := v
				cp = &copy
				break
			}
		}
		if cp == nil {
			item.mu.Unlock()
			return item.compactManagedJournal(ctx)
		}
		reader, e := item.managedJournalReader(ctx, *cp)
		name := item.name
		item.mu.Unlock()
		if e != nil {
			return e
		}
		src := object.NewStaticObjectInfo(name, cp.info.ModTime, cp.info.Size, true, nil, item.c.fremote)
		o, e := item.c.fremote.Put(ctx, reader, src)
		e = errors.Join(e, reader.Close())
		if e != nil {
			return e
		}
		if o == nil || o.Size() != cp.info.Size {
			return errors.New("background archive returned inconsistent local snapshot size")
		}
		item.mu.Lock()
		_, _, e = j.append(journalAckRecord, cp.info.MutationSeq, 0, nil)
		if e == nil {
			e = item.c.managedIO.syncFile(j.file)
		}
		if e == nil {
			item.o = o
			item.info.StagedSeq = cp.info.MutationSeq
			item.info.Dirty = item.info.MutationSeq > item.info.StagedSeq
			item._updateFingerprint()
			e = item.saveManaged()
		}
		callback := fn
		if callback == nil {
			callback = item.managedStoreFn
		}
		item.mu.Unlock()
		if e != nil {
			return e
		}
		if callback != nil {
			callback(o)
		}
	}
}

// DrainManagedArchives waits for locally accepted checkpoints only. It does not
// accept unsaved writes or perform cloud uploads.
func (c *Cache) DrainManagedArchives(ctx context.Context) error {
	c.archiveMu.RLock()
	defer c.archiveMu.RUnlock()
	c.mu.Lock()
	items := make([]*Item, 0, len(c.item))
	for _, i := range c.item {
		items = append(items, i)
	}
	c.mu.Unlock()
	for _, i := range items {
		if err := i.stageManagedJournal(ctx, nil); err != nil {
			return err
		}
	}
	return nil
}

// Namespace callers hold this reservation through local publication, so a
// worker cannot import a stale path or rebase a cache body during a rename.
func (c *Cache) BlockManagedArchives() func() {
	c.archiveMu.Lock()
	return c.archiveMu.Unlock
}

// Rebase only when all saved versions are safely accepted and there are no
// handles. Building the new cache body runs outside mutation exclusion; the
// short publication section checks that no new edits arrived in the meantime.
func (item *Item) compactManagedJournal(ctx context.Context) (err error) {
	item.mu.Lock()
	j := item.journal
	if j == nil || item.opens != 0 || item.info.Dirty || item.info.MutationSeq != item.info.StagedSeq || len(j.checkpoints) == 0 {
		item.mu.Unlock()
		return nil
	}
	cp := j.checkpoints[len(j.checkpoints)-1]
	reader, err := item.managedJournalReader(ctx, cp)
	dataPath := item.c.toOSPath(item.name)
	name := item.name
	item.mu.Unlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dataPath), ".rclone-managed-rebase-")
	if err != nil {
		reader.Close()
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = io.Copy(tmp, reader)
	err = errors.Join(err, reader.Close())
	if err == nil {
		err = item.c.managedIO.syncFile(tmp)
	}
	err = errors.Join(err, tmp.Close())
	if err != nil {
		return err
	}
	item.snapshotMu.Lock()
	defer item.snapshotMu.Unlock()
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.journal != j || item.name != name || item.opens != 0 || item.info.MutationSeq != cp.info.MutationSeq || item.info.Dirty || item.fd != nil {
		return nil
	}
	if err = item.c.managedIO.rename(tmp.Name(), dataPath); err != nil {
		return err
	}
	if err = syncParents(filepath.Dir(dataPath)); err != nil {
		return err
	}
	if _, _, err = j.append(journalRetireRecord, cp.info.MutationSeq, 0, nil); err != nil {
		return err
	}
	if err = item.c.managedIO.syncFile(j.file); err != nil {
		return err
	}
	previous := item.info
	item.info.Journal, item.info.JournalEnd = "", 0
	item.info.Rs = nil
	if item.info.Size > 0 {
		item.info.Rs.Insert(ranges.Range{Pos: 0, Size: item.info.Size})
	}
	if err = item.saveManaged(); err != nil {
		item.info = previous
		return err
	}
	path := j.file.Name()
	id := previous.Journal
	j.close()
	item.journal = nil
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = syncParents(journalRoot(item.c.metaRoot)); err != nil {
		return err
	}
	return item.releaseManagedBase(id)
}
