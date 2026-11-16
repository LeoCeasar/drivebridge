//go:build linux

package vfs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagedRenameReadyWaitsForReleasedWriter(t *testing.T) {
	v, backend, _ := newManagedFS(t, false)
	h, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	require.NoError(t, h.Flush())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.root.WaitManagedRenameReady(ctx, "file", "renamed", v.root) }()
	select {
	case err := <-done:
		_ = h.Release()
		t.Fatalf("readiness returned before final Release: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	require.Empty(t, backend.events, "readiness must not begin a namespace transaction")
	require.NoError(t, h.Release())
	require.NoError(t, <-done)
	require.Empty(t, backend.events)
	require.NoError(t, v.Rename("file", "renamed"), "readiness must release its probe reservation")
	require.Equal(t, []string{"begin", "commit", "finish"}, backend.events)
}

func TestManagedRenameReadyKeepsFlushedDuplicateWritableAndBusy(t *testing.T) {
	v, backend, _ := newManagedFS(t, false)
	h, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	defer h.Release()
	// Flush may close only one dup'd descriptor. The shared handle stays live.
	require.NoError(t, h.Flush())
	_, err = h.WriteAt([]byte("still writable"), 0)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	err = v.root.WaitManagedRenameReady(ctx, "file", "renamed", v.root)
	require.ErrorIs(t, err, ErrExternalUpdateBusy)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, backend.events)
	_, err = h.WriteAt([]byte("live"), 0)
	require.NoError(t, err)
}

func TestManagedRenameReadyHonorsCallerCancellation(t *testing.T) {
	v, backend, _ := newManagedFS(t, false)
	h, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	defer h.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = v.root.WaitManagedRenameReady(ctx, "file", "renamed", v.root)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, backend.events)
}

func TestManagedRenameReadyUnmanagedBypassesWait(t *testing.T) {
	v := &VFS{}
	dir := &Dir{vfs: v}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, dir.WaitManagedRenameReady(ctx, "file", "renamed", dir))
}
