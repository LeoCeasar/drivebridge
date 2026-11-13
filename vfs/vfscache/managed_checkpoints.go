package vfscache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

const detachedManagedPrefix = ".drivebridge-detached/"

func detachedManagedName(id string) string { return detachedManagedPrefix + id }
func isDetachedManagedName(name string) bool {
	return strings.HasPrefix(name, detachedManagedPrefix) || strings.HasPrefix(name, ".drivebridge-readers/")
}

// CheckpointManagedTree persists only WAL checkpoints. A namespace transaction
// owns these small references before cache names change; full-body import runs
// independently and resolves its destination by journal identity.
func (c *Cache) CheckpointManagedTree(ctx context.Context, name string) ([]model.ManagedCheckpoint, error) {
	refs, release, err := c.checkpointManagedTree(ctx, name, false)
	release()
	return refs, err
}

func (c *Cache) PinManagedNamespaceTree(ctx context.Context, name string) ([]model.ManagedCheckpoint, func(), error) {
	return c.checkpointManagedTree(ctx, name, true)
}

func (c *Cache) checkpointManagedTree(ctx context.Context, name string, pin bool) ([]model.ManagedCheckpoint, func(), error) {
	var pinned []*Item
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, item := range pinned {
				item.mu.Lock()
				item.namespacePins--
				item.mu.Unlock()
			}
			c.wakeManagedWorker()
		})
	}
	c.mu.Lock()
	names := make([]string, 0)
	for p := range c.item {
		if p == name || strings.HasPrefix(p, name+"/") {
			names = append(names, p)
		}
	}
	c.mu.Unlock()
	sort.Strings(names)
	var out []model.ManagedCheckpoint
	for _, p := range names {
		c.mu.Lock()
		item := c.item[p]
		c.mu.Unlock()
		if item == nil {
			continue
		}
		item.snapshotMu.Lock()
		item.mu.Lock()
		if err := ctx.Err(); err != nil {
			item.mu.Unlock()
			item.snapshotMu.Unlock()
			release()
			return nil, func() {}, err
		}
		var err error
		if item.journal != nil {
			err = item.checkpointManagedLocked()
		}
		if err == nil && item.info.Journal != "" {
			var raw []byte
			raw, err = json.Marshal(item.info)
			if err == nil {
				out = append(out, model.ManagedCheckpoint{Journal: item.info.Journal, Sequence: item.info.DurableSeq, Remote: p, Size: item.info.Size, ModTime: item.info.ModTime, CacheInfo: raw})
				if pin {
					item.namespacePins++
					pinned = append(pinned, item)
				}
			}
		}
		item.mu.Unlock()
		item.snapshotMu.Unlock()
		if err != nil {
			release()
			return nil, func() {}, err
		}
	}
	return out, release, nil
}

// DetachManagedTree retains acknowledged local saves for history after unlink
// or replacement. The internal names are never admitted into VFS directory rows.
func (c *Cache) DetachManagedTree(name string) error {
	c.mu.Lock()
	var names []string
	for p := range c.item {
		if p == name || strings.HasPrefix(p, name+"/") {
			names = append(names, p)
		}
	}
	c.mu.Unlock()
	sort.Strings(names)
	for _, p := range names {
		c.mu.Lock()
		item := c.item[p]
		c.mu.Unlock()
		if item == nil {
			continue
		}
		item.snapshotMu.Lock()
		item.mu.Lock()
		id := item.info.Journal
		item.mu.Unlock()
		if id == "" {
			item.snapshotMu.Unlock()
			publish, release, err := c.PrepareReadPublication(p)
			if err != nil {
				return err
			}
			err = publish()
			release()
			if err != nil {
				return err
			}
			continue
		}
		err := c.Rename(p, detachedManagedName(id), nil)
		item.snapshotMu.Unlock()
		if err != nil {
			return err
		}
		for _, dir := range []string{filepath.Dir(c.toOSPath(p)), filepath.Dir(c.toOSPathMeta(p)), filepath.Dir(c.toOSPath(detachedManagedName(id))), filepath.Dir(c.toOSPathMeta(detachedManagedName(id)))} {
			if err := c.syncManagedParents(dir); err != nil {
				return err
			}
		}
	}
	c.wakeManagedWorker()
	return nil
}

// Recovery runs before cache reload and before journal-pin collection. Rebuild
// named metadata from the namespace's durable references, even if the prior
// process stopped between a cache rename and its metadata publication.
func (c *Cache) recoverManagedNamespace(src, dst string, refs []model.ManagedCheckpoint) error {
	if len(refs) == 0 {
		return c.discardCommittedNamespace(src, dst)
	}
	ordered := append([]model.ManagedCheckpoint(nil), refs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return !underManaged(ordered[i].Remote, src) && underManaged(ordered[j].Remote, src)
	})
	for _, ref := range ordered {
		target, detached := detachedManagedName(ref.Journal), true
		if dst != "" && underManaged(ref.Remote, src) {
			target, detached = dst+strings.TrimPrefix(ref.Remote, src), false
		}
		if err := c.recoverManagedCheckpoint(ref, target, detached); err != nil {
			return err
		}
	}
	return nil
}
func underManaged(p, root string) bool {
	return root != "" && (p == root || strings.HasPrefix(p, root+"/"))
}

func (c *Cache) recoverManagedCheckpoint(ref model.ManagedCheckpoint, target string, detached bool) error {
	if ref.Remote == "" || clean(ref.Remote) != ref.Remote || strings.HasPrefix(ref.Remote, "/") || strings.Contains(ref.Remote, "\\") || ref.Remote == ".." || strings.HasPrefix(ref.Remote, "../") {
		return errors.New("invalid checkpoint cache path")
	}
	if _, err := journalPath(c.metaRoot, ref.Journal); err != nil {
		return err
	}
	var info Info
	if err := json.Unmarshal(ref.CacheInfo, &info); err != nil {
		return err
	}
	if info.Journal != ref.Journal || info.DurableSeq != ref.Sequence || info.Size != ref.Size {
		return errors.New("checkpoint recovery identity mismatch")
	}
	for _, name := range []string{ref.Remote, target} {
		if err := rejectRecoverySymlinks(c.root, c.metaRoot, c.toOSPath(name)); err != nil {
			return err
		}
		if err := rejectRecoverySymlinks(c.root, c.metaRoot, c.toOSPathMeta(name)); err != nil {
			return err
		}
	}
	// A completed move may already have newer acknowledgements. Never replace
	// another journal, nor discard a source whose move has not been identified.
	readInfo := func(name string) (Info, error) {
		var v Info
		b, e := os.ReadFile(c.toOSPathMeta(name))
		if e == nil {
			e = json.Unmarshal(b, &v)
		}
		return v, e
	}
	atTarget := false
	if v, err := readInfo(target); err == nil && v.Journal == ref.Journal {
		info = v
		atTarget = true
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !atTarget {
		if v, err := readInfo(ref.Remote); err == nil && v.Journal == ref.Journal {
			if err = rename(c.toOSPath(ref.Remote), c.toOSPath(target)); err != nil {
				return err
			}
			info = v
			if ref.Remote != target {
				if err = os.Remove(c.toOSPathMeta(ref.Remote)); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if atTarget && ref.Remote != target {
		if _, err := os.Stat(c.toOSPath(target)); os.IsNotExist(err) {
			if err = rename(c.toOSPath(ref.Remote), c.toOSPath(target)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	info.Detached = detached
	if err := os.MkdirAll(filepath.Dir(c.toOSPathMeta(target)), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.toOSPath(target)), 0700); err != nil {
		return err
	}
	item := &Item{c: c, name: target, info: info}
	if err := item.saveManaged(); err != nil {
		return fmt.Errorf("restore namespace checkpoint: %w", err)
	}
	return c.syncManagedDirectories(filepath.Dir(c.toOSPath(ref.Remote)), filepath.Dir(c.toOSPath(target)), filepath.Dir(c.toOSPathMeta(ref.Remote)), filepath.Dir(c.toOSPathMeta(target)))
}

// This interface deliberately separates identity routing from pathname-based Put.
type managedCheckpointArchiver interface {
	ArchiveManagedCheckpoint(context.Context, string, uint64, io.Reader, fs.ObjectInfo) (fs.Object, error)
}

func (c *Cache) UsesManagedCheckpointArchive() bool {
	_, ok := c.fremote.(managedCheckpointArchiver)
	return c.managed.Load() && ok
}
