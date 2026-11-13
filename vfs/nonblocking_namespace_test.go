package vfs

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

type checkpointManagedFS struct{ *managedTestFS }

func (f *checkpointManagedFS) ManagedSnapshotBase(context.Context, string) (string, error) {
	return "", nil
}
func (f *checkpointManagedFS) OpenManagedSnapshot(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("no base for new test file")
}
func (f *checkpointManagedFS) BindManagedNamespace(context.Context, string, []model.ManagedCheckpoint) error {
	return nil
}
func (f *checkpointManagedFS) ArchiveManagedCheckpoint(ctx context.Context, key string, seq uint64, r io.Reader, src fs.ObjectInfo) (fs.Object, error) {
	return f.Put(ctx, r, src)
}

func TestManagedRenameDoesNotWaitForUnrelatedArchive(t *testing.T) {
	run := fstest.NewRun(t)
	run.WriteObject(context.Background(), "original.md", "original local bytes", time.Now())
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	parent := &managedTestFS{Fs: run.Fremote, putBarrier: func() { once.Do(func() { close(entered); <-leave }) }}
	opts := vfscommon.Opt
	opts.CacheMode = vfscommon.CacheModeFull
	opts.CachePollInterval = 0
	opts.WriteBack = 0
	opts.HandleCaching = 0
	v, err := NewManaged(context.Background(), &checkpointManagedFS{parent}, &opts)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupVFS(t, v) })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(leave) }) }
	defer release()
	h, err := v.OpenFile("unrelated.md", os.O_CREATE|os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = h.Write([]byte("saved bytes"))
	require.NoError(t, err)
	require.NoError(t, h.Sync())
	require.NoError(t, h.Close())
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive did not start")
	}
	done := make(chan error, 1)
	go func() { done <- v.Rename("original.md", "renamed.md") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("rename waited for unrelated archive")
	}
}
