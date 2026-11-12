package vfs

import (
	"context"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestExternalReconcileDeletionWaitsForOpenReader(t *testing.T) {
	v, o := managedTestVFS(t)
	h, err := v.Open("file")
	require.NoError(t, err)
	called := false
	err = v.TryExternalReconcile(context.Background(), "file", func() (fs.Object, error) {
		called = true
		return nil, o.Remove(context.Background())
	})
	require.ErrorIs(t, err, ErrExternalUpdateBusy)
	require.False(t, called)
	require.NoError(t, h.Release())
	require.NoError(t, v.TryExternalReconcile(context.Background(), "file", func() (fs.Object, error) {
		return nil, o.Remove(context.Background())
	}))
	_, err = v.Stat("file")
	require.Error(t, err)
}

func TestExternalReconcileConflictKeepsVisibleFile(t *testing.T) {
	v, o := managedTestVFS(t)
	require.NoError(t, v.TryExternalReconcile(context.Background(), "file", func() (fs.Object, error) { return o, nil }))
	h, err := v.Open("file")
	require.NoError(t, err)
	require.NoError(t, h.Release())
}
