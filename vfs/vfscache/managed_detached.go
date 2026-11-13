package vfscache

import (
	"context"
	"os"
	"path/filepath"
)

// Removed generations already have durable archive receipts. No second full
// body compaction is needed merely to delete the derived cache.
func (item *Item) retireDetachedJournal(ctx context.Context) error {
	item.snapshotMu.Lock()
	defer item.snapshotMu.Unlock()
	item.mu.Lock()
	if err := ctx.Err(); err != nil {
		item.mu.Unlock()
		return err
	}
	j := item.journal
	if !item.info.Detached || j == nil || item.opens != 0 || item.namespacePins != 0 || item.pendingAccesses != 0 || item.info.Dirty || item.info.MutationSeq != item.info.StagedSeq {
		item.mu.Unlock()
		return nil
	}
	name := item.name
	if _, _, err := j.append(journalRetireRecord, item.info.DurableSeq, 0, nil); err != nil {
		item.mu.Unlock()
		return err
	}
	if err := item.c.managedIO.syncFile(j.file); err != nil {
		item.mu.Unlock()
		return err
	}
	for _, p := range []string{item.c.toOSPath(name), item.c.toOSPathMeta(name)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			item.mu.Unlock()
			return err
		}
		if err := item.c.syncManagedParents(filepath.Dir(p)); err != nil {
			item.mu.Unlock()
			return err
		}
	}
	if err := item.removeManagedJournalFiles(); err != nil {
		item.mu.Unlock()
		return err
	}
	item.info.Journal, item.info.JournalEnd = "", 0
	item.mu.Unlock()
	item.c.mu.Lock()
	if item.c.item[name] == item {
		delete(item.c.item, name)
	}
	item.c.mu.Unlock()
	return nil
}
