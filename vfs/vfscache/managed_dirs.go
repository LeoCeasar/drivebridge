package vfscache

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/rclone/rclone/internal/bridge/platform"
)

// The changed directory must always be synced. Its ancestors need another sync
// only when the child's directory entry is new. File identity, rather than path
// alone, detects a directory removed and recreated by cache maintenance.
func (c *Cache) syncManagedParents(dir string) error {
	c.managedDirsMu.Lock()
	defer c.managedDirsMu.Unlock()
	if c.managedDirs == nil {
		c.managedDirs = map[string]os.FileInfo{}
	}
	if len(c.managedDirs) > 8192 {
		c.managedDirs = map[string]os.FileInfo{}
	}
	syncDir := c.managedIO.syncDir
	if syncDir == nil {
		syncDir = c.syncManagedDirectory
	}
	type checked struct {
		path string
		info os.FileInfo
	}
	var checkedDirs []checked
	for {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		previous := c.managedDirs[dir]
		if err := syncDir(dir); err != nil {
			return err
		}
		checkedDirs = append(checkedDirs, checked{dir, info})
		if previous != nil && os.SameFile(previous, info) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for _, v := range checkedDirs {
		c.managedDirs[v.path] = v.info
	}
	return nil
}

// Frequently changed cache directories share a bounded set of read-only
// descriptors. This avoids repeatedly opening/closing the same directory while
// retaining exactly the same fsync durability boundary.
func (c *Cache) syncManagedDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return platform.SyncDir(dir)
	}
	if c.managedDirHandles == nil {
		c.managedDirHandles = map[string]*os.File{}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	f := c.managedDirHandles[dir]
	if f != nil {
		previous, e := f.Stat()
		if e != nil || !os.SameFile(previous, info) {
			_ = f.Close()
			delete(c.managedDirHandles, dir)
			f = nil
		}
	}
	if f == nil {
		if len(c.managedDirHandles) >= 64 {
			for old, handle := range c.managedDirHandles {
				_ = handle.Close()
				delete(c.managedDirHandles, old)
				break
			}
		}
		f, err = os.Open(dir)
		if err != nil {
			return err
		}
		c.managedDirHandles[dir] = f
	}
	return platform.SyncFile(f)
}

func (c *Cache) closeManagedDirectories() {
	c.managedDirsMu.Lock()
	defer c.managedDirsMu.Unlock()
	for name, handle := range c.managedDirHandles {
		_ = handle.Close()
		delete(c.managedDirHandles, name)
	}
	c.managedDirs = nil
}

func (c *Cache) syncManagedDirectories(dirs ...string) error {
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if err := c.syncManagedParents(dir); err != nil {
			return err
		}
	}
	return nil
}
