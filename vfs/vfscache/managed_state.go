package vfscache

import (
	"encoding/json"
	"fmt"
	"github.com/rclone/rclone/fs"
	"os"
	"path/filepath"
	"strings"
)

func (c *Cache) ManagedPaths() (dataRoot, metaRoot string) { return c.root, c.metaRoot }

// ManagedLocalFile resolves a pending local save before an unknown-size overlay
// placeholder attempts cloud materialization. A completed background import can
// also provide its object to a virtual node reconstructed during startup.
func (c *Cache) ManagedLocalFile(name string) (fs.Object, int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item := c.item[clean(name)]
	if item == nil || !c.managed.Load() {
		return nil, 0, false
	}
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.journal != nil || item.info.MutationSeq > 0 && item.info.MutationSeq == item.info.StagedSeq && item.o != nil {
		return item.o, item.info.Size, true
	}
	return nil, 0, false
}

// ManagedLocalSaveStatus separates local durability from background preparation.
func (c *Cache) ManagedLocalSaveStatus() (saved bool, preparing int, failed []string) {
	saved = true
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, item := range c.item {
		item.mu.Lock()
		if item.info.MutationSeq > item.info.DurableSeq || item.journal == nil && item.info.Dirty {
			saved = false
		}
		if item.info.MutationSeq > item.info.StagedSeq {
			preparing++
		}
		if item.managedStageError != nil {
			failed = append(failed, name)
		}
		item.mu.Unlock()
	}
	return
}
func (c *Cache) ManagedPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, i := range c.item {
		i.mu.Lock()
		pending := i.info.Dirty || i.info.MutationSeq > i.info.StagedSeq || i.pendingAccesses != 0 || i.beingReset || i.closing != nil || i.managedStaging || i.managedStageError != nil
		i.mu.Unlock()
		if pending {
			return true
		}
	}
	running, queued := c.writeback.Stats()
	return running+queued != 0
}

// ManagedMetadataPending inspects an offline cache under the caller's per-mount
// lock. It never deletes recovery data; corrupt/unknown state fails closed.
func ManagedMetadataPending(dataRoot, metaRoot string) (pending bool, err error) {
	if dataRoot == "" || metaRoot == "" {
		return false, fmt.Errorf("managed cache paths are required")
	}
	metadata := map[string]Info{}
	walk := func(root string, fn func(string, os.FileInfo) error) error {
		return filepath.Walk(root, func(p string, fi os.FileInfo, e error) error {
			if e != nil {
				if p == root && os.IsNotExist(e) {
					return nil
				}
				return e
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in managed cache: %s", p)
			}
			if fi.IsDir() {
				return nil
			}
			return fn(p, fi)
		})
	}
	err = walk(metaRoot, func(p string, fi os.FileInfo) error {
		if strings.HasPrefix(fi.Name(), managedTempPrefix) {
			pending = true
			return nil
		}
		if fi.Size() > 16<<20 {
			return fmt.Errorf("oversized managed metadata")
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		var info Info
		if e = json.Unmarshal(b, &info); e != nil {
			return fmt.Errorf("corrupt managed metadata %s: %w", p, e)
		}
		if info.DurableSeq > info.MutationSeq || info.StagedSeq > info.MutationSeq {
			return fmt.Errorf("invalid managed sequence")
		}
		relative, e := filepath.Rel(metaRoot, p)
		if e != nil {
			return e
		}
		metadata[relative] = info
		if info.Dirty || info.MutationSeq > info.StagedSeq {
			pending = true
		}
		return nil
	})
	if err != nil {
		return pending, err
	}
	linked := map[string]bool{}
	for name, info := range metadata {
		if info.Journal == "" {
			continue
		}
		p, e := journalPath(metaRoot, info.Journal)
		if e != nil {
			return true, e
		}
		if linked[info.Journal] {
			return true, fmt.Errorf("local-save journal linked more than once")
		}
		linked[info.Journal] = true
		_, recovered, _, e := scanManagedJournal(p, info)
		if e != nil {
			return true, e
		}
		if recovered.Dirty {
			pending = true
		}
		if _, e = os.Stat(filepath.Join(dataRoot, name)); e != nil {
			return true, fmt.Errorf("local-save base missing: %w", e)
		}
	}
	if err = walk(journalRoot(metaRoot), func(p string, fi os.FileInfo) error {
		id := fi.Name()
		if linked[id] {
			return nil
		}
		if _, e := journalPath(metaRoot, id); e != nil {
			return e
		}
		j, recovered, _, e := scanManagedJournal(p, Info{Journal: id})
		if e != nil {
			return e
		}
		if j.scanEnd != fi.Size() || recovered.Dirty || j.uncommitted {
			return fmt.Errorf("unlinked local-save journal requires recovery")
		}
		return nil
	}); err != nil {
		return true, err
	}
	err = walk(dataRoot, func(p string, fi os.FileInfo) error {
		relative, e := filepath.Rel(dataRoot, p)
		if e != nil {
			return e
		}
		info, ok := metadata[relative]
		if !ok || info.Journal == "" && info.Size != fi.Size() {
			pending = true
		}
		return nil
	})
	return pending, err
}
