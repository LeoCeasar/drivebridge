package vfscache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Startup has already validated the journals and holds the mount ownership
// lock. Only unlinked, fully acknowledged logs can be collected here. This
// also recovers a crash after namespace commit removed its derived cache.
func (c *Cache) reconcileManagedBasePins(ctx context.Context) error {
	linked := map[string]bool{}
	err := filepath.Walk(c.metaRoot, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || strings.HasPrefix(fi.Name(), managedTempPrefix) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var info Info
		if err = json.Unmarshal(data, &info); err != nil {
			return err
		}
		if info.Journal != "" {
			linked[info.Journal] = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(journalRoot(c.metaRoot))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	removed := false
	for _, entry := range entries {
		if !linked[entry.Name()] {
			// ManagedMetadataPending proved that every remaining byte of an
			// unlinked journal has already been accepted into the durable outbox.
			if err = os.Remove(filepath.Join(journalRoot(c.metaRoot), entry.Name())); err != nil {
				return err
			}
			removed = true
		}
	}
	if removed {
		if err = syncParents(journalRoot(c.metaRoot)); err != nil {
			return err
		}
	}
	if retain, ok := c.fremote.(managedSnapshotRetention); ok {
		return retain.ReconcileManagedSnapshots(ctx, linked)
	}
	return nil
}

// Caller has durably removed/replaced the metadata reference first. Keeping
// the base pinned until the log is gone makes interrupted cleanup recoverable.
func (item *Item) removeManagedJournalFiles() error {
	id := item.info.Journal
	if id == "" {
		return nil
	}
	p, err := journalPath(item.c.metaRoot, id)
	if err != nil {
		return err
	}
	if item.journal != nil {
		item.journal.close()
		item.journal = nil
	}
	if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = syncParents(journalRoot(item.c.metaRoot)); err != nil {
		return err
	}
	return item.releaseManagedBase(id)
}
