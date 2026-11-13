package vfscache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// PrepareReadPublication pins a clean generation against eviction while an
// already prepared metadata update runs. Readers may continue using it. The
// returned publish function moves that generation aside without stopping its
// downloader or changing its object; new opens then get a separate Item.
// The caller excludes writers and serializes new reader construction with publish.
func (c *Cache) PrepareReadPublication(name string) (publish func() error, release func(), err error) {
	name = clean(name)
	c.mu.Lock()
	item, _ := c._get(name)
	item.mu.Lock()
	id := item.writeBackID
	item.mu.Unlock()
	idle, checkErr := c.writeback.WithIdle(id, func() error {
		item.mu.Lock()
		defer item.mu.Unlock()
		if item.info.Dirty || item.info.MutationSeq > item.info.StagedSeq || item.info.Journal != "" || item.beingReset || item.closing != nil || item.managedStaging || item.managedStageError != nil {
			return ErrBusy
		}
		item.pendingAccesses++
		return nil
	})
	c.mu.Unlock()
	if !idle {
		return nil, nil, ErrBusy
	}
	if checkErr != nil {
		return nil, nil, checkErr
	}
	var once sync.Once
	release = func() { once.Do(func() { item.postAccess(); item.removeCleanReadGeneration() }) }
	hidden := ".drivebridge-readers/" + uuid.NewString()
	if _, err = c.createItemDir(hidden); err != nil {
		release()
		return nil, nil, err
	}
	publish = func() error {
		item.snapshotMu.Lock()
		defer item.snapshotMu.Unlock()
		c.mu.Lock()
		defer c.mu.Unlock()
		item.mu.Lock()
		defer item.mu.Unlock()
		if c.item[name] != item {
			return ErrBusy
		}
		// Only readers can have appeared since preparation; they neither dirty this
		// generation nor replace its immutable object.
		if item.info.Dirty || item.info.MutationSeq > item.info.StagedSeq || item.info.Journal != "" {
			return ErrBusy
		}
		if item.opens == 0 && item.pendingAccesses == 1 {
			// Only the publication pin remains. No handle needs this derived body.
			for _, p := range []string{c.toOSPath(name), c.toOSPathMeta(name)} {
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			delete(c.item, name)
			return nil
		}
		oldData, newData := c.toOSPath(name), c.toOSPath(hidden)
		moved := false
		if err := os.Rename(oldData, newData); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			moved = true
		}
		oldMeta, newMeta := c.toOSPathMeta(name), c.toOSPathMeta(hidden)
		if err := os.Rename(oldMeta, newMeta); err != nil && !errors.Is(err, os.ErrNotExist) {
			if moved {
				_ = os.Rename(newData, oldData)
			}
			return err
		}
		item.name = hidden
		item.info.Detached = true
		item.managedDirsDurable = false
		delete(c.item, name)
		c.item[hidden] = item
		// No saved user bytes live in this clean generation. The detached marker
		// keeps startup from exposing its internal cache name as a virtual file.
		if moved {
			if err := item.saveManaged(); err != nil {
				return err
			}
			for _, dir := range []string{filepath.Dir(oldData), filepath.Dir(newData), filepath.Dir(oldMeta), filepath.Dir(newMeta)} {
				if err := c.syncManagedParents(dir); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return publish, release, nil
}

// PinReadGeneration protects a lazy read handle's complete cached body until
// close, even before Item.Open has raised the normal open count.
func (item *Item) PinReadGeneration() func() {
	item.preAccess()
	var once sync.Once
	return func() { once.Do(func() { item.postAccess(); item.removeCleanReadGeneration() }) }
}

// A detached clean read generation has no recovery responsibility. Remove it
// immediately after the last lazy/open reader releases its pin.
func (item *Item) removeCleanReadGeneration() {
	c := item.c
	c.mu.Lock()
	defer c.mu.Unlock()
	item.mu.Lock()
	defer item.mu.Unlock()
	if !strings.HasPrefix(item.name, ".drivebridge-readers/") || !item.info.Detached || item.opens != 0 || item.pendingAccesses != 0 || item.info.Dirty || item.info.Journal != "" || item.managedStaging || item.fd != nil {
		return
	}
	for _, p := range []string{c.toOSPath(item.name), c.toOSPathMeta(item.name)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	if c.item[item.name] == item {
		delete(c.item, item.name)
	}
}
