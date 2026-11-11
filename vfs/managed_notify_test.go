package vfs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/require"
)

func TestManagedDirectoryNotificationExpiresLocalCacheAndReentersSafely(t *testing.T) {
	v, _, r := newManagedFS(t, false)
	before, err := v.root.ReadDirAll()
	require.NoError(t, err)
	require.Len(t, before, 1)
	data := "newly indexed"
	_, err = r.Fremote.Put(context.Background(), strings.NewReader(data), object.NewStaticObjectInfo("new.md", time.Now(), int64(len(data)), true, nil, r.Fremote))
	require.NoError(t, err)
	calls := 0
	v.SetManagedNativeNotifier(func(path string) {
		calls++
		require.Equal(t, "/", path)
		rows, e := v.root.ReadDirAll()
		require.NoError(t, e)
		require.Len(t, rows, 2)
	})
	v.NotifyManagedDirectoryChanged("")
	require.Equal(t, 1, calls)
	v.SetManagedNativeNotifier(nil)
	v.NotifyManagedDirectoryChanged("")
	require.Equal(t, 1, calls)
}
