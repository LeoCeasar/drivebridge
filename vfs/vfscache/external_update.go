package vfscache

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("cache item busy")

func (c *Cache) EnableManagedSync() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.managed.Load() {
		return true
	}
	for _, item := range c.item {
		item.mu.Lock()
		busy := item.opens != 0 || item.info.Dirty || item.pendingAccesses != 0 || item.closing != nil || item.graceTimer != nil
		item.mu.Unlock()
		if busy {
			return false
		}
	}
	running, queued := c.writeback.Stats()
	if running+queued != 0 {
		return false
	}
	c.opt.HandleCaching = 0
	c.managed.Store(true)
	return true
}

// InvalidateClean combines queue inspection, item-state inspection and removal.
// Cache -> writeback -> Item is the lock order; no running store is canceled.
func (c *Cache) InvalidateClean(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	name = clean(name)
	item := c.item[name]
	if item == nil {
		return nil
	}
	item.mu.Lock()
	id := item.writeBackID
	item.mu.Unlock()
	idle, err := c.writeback.WithIdle(id, func() error {
		item.mu.Lock()
		defer item.mu.Unlock()
		if item.info.Dirty || item.opens != 0 || item.pendingAccesses != 0 || item.beingReset || item.closing != nil || item.graceTimer != nil || item.managedStaging || item.managedStageError != nil {
			return ErrBusy
		}
		for _, p := range []string{c.toOSPath(name), c.toOSPathMeta(name)} {
			if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
				return e
			}
		}
		if item.info.Journal != "" {
			if e := syncParents(filepath.Dir(c.toOSPathMeta(name))); e != nil {
				return e
			}
			if e := item.removeManagedJournalFiles(); e != nil {
				return e
			}
		}
		delete(c.item, name)
		return nil
	})
	if !idle {
		return ErrBusy
	}
	return err
}
