package vfscache

import (
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManagedDirectoryDurabilityDoesNotResyncUnchangedAncestors(t *testing.T) {
	c, _, _ := journalItem(t, "")
	dir := filepath.Join(c.root, "new-parent", "child")
	require.NoError(t, os.MkdirAll(dir, 0700))
	calls := map[string]int{}
	c.managedIO.syncDir = func(p string) error { calls[p]++; return nil }
	require.NoError(t, c.syncManagedParents(dir))
	parent := filepath.Dir(dir)
	before := calls[parent]
	require.Positive(t, before)
	for n := 0; n < 10; n++ {
		require.NoError(t, c.syncManagedParents(dir))
	}
	require.Equal(t, before, calls[parent], "unchanged ancestors must not fsync on every save")
	require.Equal(t, 11, calls[dir], "changed directory itself must always be durable")
	renamed := dir + "-old"
	require.NoError(t, os.Rename(dir, renamed))
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, c.syncManagedParents(dir))
	require.Equal(t, before+1, calls[parent], "recreated directory identity needs parent durability again")
}

func TestManagedDirectoryHandlesAreReusedAndClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory sync is a platform no-op")
	}
	c, _, _ := journalItem(t, "")
	dir := filepath.Join(c.root, "fd-reuse")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, c.syncManagedParents(dir))
	first := c.managedDirHandles[dir]
	require.NotNil(t, first)
	require.NoError(t, c.syncManagedParents(dir))
	require.Same(t, first, c.managedDirHandles[dir])
	c.closeManagedDirectories()
	_, err := first.Stat()
	require.Error(t, err)
	require.Empty(t, c.managedDirHandles)
}
