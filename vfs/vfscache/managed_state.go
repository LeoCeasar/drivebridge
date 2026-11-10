package vfscache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (c *Cache) ManagedPaths() (dataRoot, metaRoot string) { return c.root, c.metaRoot }
func (c *Cache) ManagedPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, i := range c.item {
		i.mu.Lock()
		pending := i.info.Dirty || i.info.MutationSeq > i.info.StagedSeq || i.pendingAccesses != 0 || i.beingReset || i.closing != nil
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
	err = walk(dataRoot, func(p string, fi os.FileInfo) error {
		relative, e := filepath.Rel(dataRoot, p)
		if e != nil {
			return e
		}
		info, ok := metadata[relative]
		if !ok || info.Size != fi.Size() {
			pending = true
		}
		return nil
	})
	return pending, err
}
