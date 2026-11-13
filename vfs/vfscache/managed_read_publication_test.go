package vfscache

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/require"
)

func TestReadPublicationSeparatesCleanGenerations(t *testing.T) {
	_, c := newItemTestCache(t)
	require.True(t, c.EnableManagedSync())
	old := object.NewMemoryObject("file", time.Now(), []byte("old-complete-body"))
	item := c.Item("file")
	require.NoError(t, item.Open(old))
	defer func() { _ = item.Close(nil) }()
	body := make([]byte, old.Size())
	_, err := item.ReadAt(body, 0)
	require.NoError(t, err)
	publish, release, err := c.PrepareReadPublication("file")
	require.NoError(t, err)
	defer release()
	require.NoError(t, publish())
	require.NotSame(t, item, c.Item("file"))
	_, err = item.ReadAt(body, 0)
	require.NoError(t, err)
	require.Equal(t, "old-complete-body", string(body))
	item.mu.Lock()
	require.True(t, item.info.Detached)
	item.mu.Unlock()
	next := c.Item("file")
	newer := object.NewMemoryObject("file", time.Now().Add(time.Second), []byte("new-body"))
	require.NoError(t, next.Open(newer))
	defer func() { _ = next.Close(nil) }()
	body = make([]byte, newer.Size())
	_, err = next.ReadAt(body, 0)
	require.NoError(t, err)
	require.Equal(t, "new-body", string(body))
}

func TestReadPublicationRejectsDirtyAndUploading(t *testing.T) {
	c, item := managedItem(t)
	_, _, err := c.PrepareReadPublication("managed")
	require.ErrorIs(t, err, ErrBusy)
	require.NoError(t, item.Close(nil))
	c.writeback.SetID(&item.writeBackID)
	entered, leave := make(chan struct{}), make(chan struct{})
	c.writeback.Add(item.writeBackID, "managed", 5, true, func(context.Context) error { close(entered); <-leave; return nil })
	<-entered
	_, _, err = c.PrepareReadPublication("managed")
	close(leave)
	c.writeback.Remove(item.writeBackID)
	require.ErrorIs(t, err, ErrBusy)
}

func TestLazyReadPinProtectsGenerationFromReset(t *testing.T) {
	_, c := newItemTestCache(t)
	require.True(t, c.EnableManagedSync())
	item := c.Item("file")
	old := object.NewMemoryObject("file", time.Now(), []byte("old"))
	require.NoError(t, item.Open(old))
	require.NoError(t, item.Close(nil))
	release := item.PinReadGeneration()
	removed, _ := item.RemoveNotInUse(0, false)
	require.False(t, removed)
	result, _, err := item.Reset()
	require.NoError(t, err)
	require.Equal(t, SkippedPendingAccess, result)
	release()
	removed, _ = item.RemoveNotInUse(0, false)
	require.True(t, removed)
}

func TestReadPublicationDropsUnreferencedBody(t *testing.T) {
	_, c := newItemTestCache(t)
	require.True(t, c.EnableManagedSync())
	item := c.Item("file")
	require.NoError(t, item.Open(object.NewMemoryObject("file", time.Now(), []byte("old"))))
	require.NoError(t, item.Close(nil))
	publish, release, err := c.PrepareReadPublication("file")
	require.NoError(t, err)
	require.NoError(t, publish())
	release()
	require.Empty(t, c.item)
}
