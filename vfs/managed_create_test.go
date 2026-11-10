package vfs

import (
	"context"
	"os"
	"path"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/require"
)

func TestManagedCreateWithOpenParentDirectory(t *testing.T) {
	for _, parent := range []string{"", "folder"} {
		t.Run(parent, func(t *testing.T) {
			v, _ := managedTestVFS(t)
			d := v.root
			if parent != "" {
				var err error
				d, err = d.Mkdir(parent)
				require.NoError(t, err)
			}
			reader, err := d.Open(os.O_RDONLY)
			require.NoError(t, err)
			defer reader.Release()
			_, err = reader.Readdir(-1)
			require.NoError(t, err)
			h, err := v.OpenFile(path.Join(parent, "sync-check.txt"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			require.NoError(t, err, "enumerating a parent directory must not deny a new child create")
			_, err = h.Write([]byte("synthetic local copy"))
			require.NoError(t, err)
			require.NoError(t, h.Close())
			node, err := v.Stat(path.Join(parent, "sync-check.txt"))
			require.NoError(t, err)
			require.Equal(t, int64(len("synthetic local copy")), node.Size())
			_, err = d.Mkdir("new-child-directory")
			require.NoError(t, err, "parent enumeration must also permit a new child directory")
		})
	}
}

func TestManagedCreateStillRejectsExactFileHandleAndExternalReservation(t *testing.T) {
	v, object := managedTestVFS(t)
	reader, err := v.Open("file")
	require.NoError(t, err)
	_, err = v.root.Create("file", os.O_CREATE|os.O_WRONLY)
	require.ErrorIs(t, err, ErrExternalUpdateBusy)
	require.NoError(t, reader.Release())
	entered, leave := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { close(entered); <-leave; return object, nil })
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("publication failed early: %v", err)
	}
	_, err = v.root.Create("file", os.O_CREATE|os.O_WRONLY)
	require.ErrorIs(t, err, ErrExternalUpdateBusy)
	close(leave)
	require.NoError(t, <-done)
}

func TestManagedParentDirectoryReaderRetainsPublicationAndRenameBarriers(t *testing.T) {
	v, object := managedTestVFS(t)
	reader, err := v.root.Open(os.O_RDONLY)
	require.NoError(t, err)
	defer reader.Release()
	called := false
	err = v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { called = true; return object, nil })
	require.ErrorIs(t, err, ErrExternalUpdateBusy)
	require.False(t, called, "ancestor directory enumeration still excludes external publication")
	require.ErrorIs(t, v.Rename("file", "renamed"), ErrExternalUpdateBusy)
	node, err := v.Stat("file")
	require.NoError(t, err)
	require.ErrorIs(t, node.Remove(), ErrExternalUpdateBusy)
}
