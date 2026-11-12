package vfscache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/platform"
)

// Called only during managed cache construction, before item reload. The overlay
// proves its immutable source/destination snapshots were committed durably first.
func (c *Cache) discardCommittedNamespace(src, dst string) error {
	for _, name := range []string{src, dst} {
		if name == "" {
			continue
		}
		if clean(name) == "" || clean(name) == "." || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) || filepath.VolumeName(filepath.FromSlash(name)) != "" || (len(name) > 1 && name[1] == ':') {
			return fmt.Errorf("unsafe namespace recovery path %q", name)
		}
		for _, p := range []string{c.toOSPath(name), c.toOSPathMeta(name)} {
			if e := rejectRecoverySymlinks(c.root, c.metaRoot, p); e != nil {
				return e
			}
			if e := os.RemoveAll(p); e != nil {
				return e
			}
			parent := filepath.Dir(p)
			for {
				_, e := os.Stat(parent)
				if os.IsNotExist(e) {
					next := filepath.Dir(parent)
					if next == parent {
						return e
					}
					parent = next
					continue
				}
				if e != nil {
					return e
				}
				if e := platform.SyncDir(parent); e != nil {
					return e
				}
				break
			}
		}
	}
	return nil
}

// FlushManagedTree stages only cached descendants and never materializes a tree.
func (c *Cache) FlushManagedTree(ctx context.Context, name string, fn func(string, fs.Object)) error {
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
		_, e := c.FlushManagedPath(ctx, p, func(o fs.Object) {
			if fn != nil {
				fn(p, o)
			}
		})
		if e != nil {
			return e
		}
	}
	return nil
}

// RenameManagedTree runs only after durable namespace commit under VFS admission.
// Any failure retains the committed journal for startup reconciliation.
func (c *Cache) RenameManagedTree(src, dst string, resolve func(string) (fs.Object, error)) error {
	c.mu.Lock()
	var dests, sources []string
	for p := range c.item {
		if p == dst || strings.HasPrefix(p, dst+"/") {
			dests = append(dests, p)
		}
		if p == src || strings.HasPrefix(p, src+"/") {
			sources = append(sources, p)
		}
	}
	c.mu.Unlock()
	sort.Strings(dests)
	sort.Strings(sources)
	for _, p := range dests {
		if e := c.InvalidateClean(p); e != nil {
			return e
		}
	}
	for _, p := range sources {
		target := dst + strings.TrimPrefix(p, src)
		o, e := resolve(target)
		if e != nil {
			return e
		}
		if e = c.Rename(p, target, o); e != nil {
			return e
		}
		for _, dir := range []string{filepath.Dir(c.toOSPath(p)), filepath.Dir(c.toOSPath(target)), filepath.Dir(c.toOSPathMeta(p)), filepath.Dir(c.toOSPathMeta(target))} {
			if e = syncParents(dir); e != nil {
				return e
			}
		}
	}
	return nil
}

func rejectRecoverySymlinks(dataRoot, metaRoot, p string) error {
	root := dataRoot
	if strings.HasPrefix(p, metaRoot+string(filepath.Separator)) {
		root = metaRoot
	}
	rel, e := filepath.Rel(root, p)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("recovery path escapes cache")
	}
	current := root
	parts := append([]string{""}, strings.Split(rel, string(filepath.Separator))...)
	for _, part := range parts {
		current = filepath.Join(current, part)
		fi, e := os.Lstat(current)
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in cache recovery path")
		}
	}
	return nil
}
