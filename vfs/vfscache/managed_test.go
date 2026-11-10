package vfscache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func managedItem(t *testing.T) (*Cache, *Item) {
	t.Helper()
	_, c := newItemTestCache(t)
	require.True(t, c.EnableManagedSync())
	i, _ := c.get("managed")
	require.NoError(t, i.Open(nil))
	_, e := i.WriteAt([]byte("first"), 0)
	require.NoError(t, e)
	return c, i
}
func TestManagedFlushSyncFailurePropagates(t *testing.T) {
	c, i := managedItem(t)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(f *os.File) error {
		if !strings.Contains(f.Name(), ".rclone-managed-meta-") {
			return syscall.ENOSPC
		}
		return real(f)
	}
	_, e := i.FlushForManagedRename(context.Background(), nil)
	require.ErrorIs(t, e, syscall.ENOSPC)
	i.mu.Lock()
	require.True(t, i.info.Dirty)
	require.Less(t, i.info.DurableSeq, i.info.MutationSeq)
	i.mu.Unlock()
	c.managedIO.syncFile = real
	require.NoError(t, i.Close(nil))
}
func TestRepeatedFlushDoesNotDuplicateGeneration(t *testing.T) {
	_, i := managedItem(t)
	var stores atomic.Int32
	callback := func(fs.Object) { stores.Add(1) }
	o, e := i.FlushForManagedRename(context.Background(), callback)
	require.NoError(t, e)
	require.Equal(t, int64(5), o.Size())
	_, e = i.FlushForManagedRename(context.Background(), callback)
	require.NoError(t, e)
	require.Equal(t, int32(1), stores.Load())
	i.mu.Lock()
	require.Equal(t, i.info.MutationSeq, i.info.DurableSeq)
	require.Equal(t, i.info.MutationSeq, i.info.StagedSeq)
	i.mu.Unlock()
	require.NoError(t, i.Close(nil))
}
func TestManagedMetadataFailureKeepsDirtyCheckpoint(t *testing.T) {
	c, i := managedItem(t)
	real := c.managedIO.rename
	c.managedIO.rename = func(string, string) error { return syscall.EIO }
	_, e := i.FlushForManagedRename(context.Background(), nil)
	require.ErrorIs(t, e, syscall.EIO)
	i.mu.Lock()
	require.True(t, i.info.Dirty)
	require.Equal(t, uint64(0), i.info.DurableSeq)
	i.mu.Unlock()
	c.managedIO.rename = real
	require.NoError(t, i.Close(nil))
}
func TestExternalUpdateRejectsDirtyOrUploading(t *testing.T) {
	c, i := managedItem(t)
	require.ErrorIs(t, c.InvalidateClean("managed"), ErrBusy)
	require.NoError(t, i.Close(nil))
	c.writeback.SetID(&i.writeBackID)
	entered, leave := make(chan struct{}), make(chan struct{})
	c.writeback.Add(i.writeBackID, "managed", 5, true, func(context.Context) error { close(entered); <-leave; return nil })
	<-entered
	require.ErrorIs(t, c.InvalidateClean("managed"), ErrBusy)
	close(leave)
	c.writeback.Remove(i.writeBackID)
	require.NoError(t, c.InvalidateClean("managed"))
	require.Nil(t, c.item["managed"])
}
func TestManagedSnapshotExcludesConcurrentWrite(t *testing.T) {
	c, i := managedItem(t)
	real := c.managedIO.syncFile
	entered, leave := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	c.managedIO.syncFile = func(f *os.File) error {
		if !strings.Contains(f.Name(), ".rclone-managed-meta-") && once.CompareAndSwap(false, true) {
			close(entered)
			<-leave
		}
		return real(f)
	}
	done := make(chan error, 1)
	go func() { _, e := i.FlushForManagedRename(context.Background(), nil); done <- e }()
	<-entered
	require.False(t, i.snapshotMu.TryLock(), "mutation lock must cover data fsync and stage")
	writeDone := make(chan error, 1)
	go func() { _, e := i.WriteAt([]byte("later"), 0); writeDone <- e }()
	close(leave)
	require.NoError(t, <-done)
	require.NoError(t, <-writeDone)
	i.mu.Lock()
	require.True(t, i.info.Dirty)
	require.Greater(t, i.info.MutationSeq, i.info.StagedSeq)
	i.mu.Unlock()
	c.managedIO.syncFile = real
	require.NoError(t, i.Close(nil))
}

func TestManagedOfflinePendingFailsClosed(t *testing.T) {
	c, i := managedItem(t)
	pending, e := ManagedMetadataPending(c.root, c.metaRoot)
	require.NoError(t, e)
	require.True(t, pending)
	require.NoError(t, i.Close(nil))
	pending, e = ManagedMetadataPending(c.root, c.metaRoot)
	require.NoError(t, e)
	require.False(t, pending)
	require.NoError(t, os.WriteFile(c.toOSPathMeta("managed"), []byte("broken"), 0600))
	_, e = ManagedMetadataPending(c.root, c.metaRoot)
	require.Error(t, e)
}
func TestManagedOfflineOrphanIsPending(t *testing.T) {
	c, i := managedItem(t)
	require.NoError(t, i.Close(nil))
	require.NoError(t, os.WriteFile(c.toOSPath("orphan"), []byte("unindexed"), 0600))
	pending, e := ManagedMetadataPending(c.root, c.metaRoot)
	require.NoError(t, e)
	require.True(t, pending)
}

func TestManagedCommittedReplayPrecedesDirtyCacheReload(t *testing.T) {
	r, c := newItemTestCache(t)
	require.True(t, c.EnableManagedSync())
	for _, name := range []string{"source", "destination"} {
		i, _ := c.get(name)
		require.NoError(t, i.Open(nil))
		_, e := i.WriteAt([]byte("stale cache"), 0)
		require.NoError(t, e)
		require.NoError(t, i.closeCache(nil))
	}
	r.WriteObject(context.Background(), "destination", "committed snapshot", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	recovered, e := NewManaged(ctx, c.fremote, c.opt, nil, func(_ context.Context, reconcile func(string, string) error) error {
		called = true
		require.NoError(t, reconcile("source", "destination"))
		_, e := os.Stat(c.toOSPath("destination"))
		require.True(t, os.IsNotExist(e))
		return nil
	})
	require.NoError(t, e)
	require.True(t, called)
	require.Empty(t, recovered.item)
	checkObject(t, r, "destination", "committed snapshot")
	require.NoError(t, recovered.CleanUp())
}
func TestManagedReplayFailurePreservesRecoverableCache(t *testing.T) {
	c, i := managedItem(t)
	require.NoError(t, i.closeCache(nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, e := NewManaged(ctx, c.fremote, c.opt, nil, func(context.Context, func(string, string) error) error { return syscall.EIO })
	require.ErrorIs(t, e, syscall.EIO)
	_, e = os.Stat(c.toOSPath("managed"))
	require.NoError(t, e)
}

func TestManagedRecoveryRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	c := &Cache{root: filepath.Join(base, "data"), metaRoot: filepath.Join(base, "meta")}
	require.NoError(t, os.MkdirAll(c.root, 0700))
	require.NoError(t, os.MkdirAll(c.metaRoot, 0700))
	sentinel := filepath.Join(base, "escaped")
	require.NoError(t, os.WriteFile(sentinel, []byte("outside cache"), 0600))
	require.Error(t, c.discardCommittedNamespace("../escaped", ""))
	b, e := os.ReadFile(sentinel)
	require.NoError(t, e)
	require.Equal(t, "outside cache", string(b))
}
