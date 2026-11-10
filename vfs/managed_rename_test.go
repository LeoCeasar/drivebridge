package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

type managedTestFS struct {
	fs.Fs
	mu             sync.Mutex
	src, dst       string
	frozen         bool
	events         []string
	materialized   atomic.Int32
	removed        atomic.Int32
	unknown        bool
	materializeErr error
	renameErr      error
	finishErr      error
	putBarrier     func()
	removeBarrier  func()
	replay         func(func(string, string) error) error
}
type managedTestObject struct {
	fs.Object
	owner *managedTestFS
}

func (o *managedTestObject) Size() int64 {
	if o.owner.unknown {
		return -1
	}
	return o.Object.Size()
}
func (o *managedTestObject) Remove(ctx context.Context) error {
	o.owner.removed.Add(1)
	if o.owner.removeBarrier != nil {
		o.owner.removeBarrier()
	}
	return o.Object.Remove(ctx)
}
func (f *managedTestFS) NewObject(ctx context.Context, n string) (fs.Object, error) {
	o, e := f.Fs.NewObject(ctx, n)
	if e != nil {
		return nil, e
	}
	return &managedTestObject{o, f}, nil
}
func (f *managedTestFS) List(ctx context.Context, n string) (fs.DirEntries, error) {
	entries, e := f.Fs.List(ctx, n)
	for i, v := range entries {
		if o, ok := v.(fs.Object); ok {
			entries[i] = &managedTestObject{o, f}
		}
	}
	return entries, e
}
func (f *managedTestFS) Put(ctx context.Context, r io.Reader, src fs.ObjectInfo, opts ...fs.OpenOption) (fs.Object, error) {
	if f.putBarrier != nil {
		f.putBarrier()
	}
	return f.Fs.Put(ctx, r, src, opts...)
}
func (f *managedTestFS) MaterializeManaged(ctx context.Context, n string) (fs.Object, error) {
	f.materialized.Add(1)
	if f.materializeErr != nil {
		return nil, f.materializeErr
	}
	return f.Fs.NewObject(ctx, n)
}
func (f *managedTestFS) BeginManagedRename(_ context.Context, src, dst string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.src, f.dst, f.frozen = src, dst, true
	f.events = append(f.events, "begin")
	return "transaction", nil
}
func (f *managedTestFS) RenameManaged(ctx context.Context, _ string) (fs.Object, error) {
	f.mu.Lock()
	f.events = append(f.events, "commit")
	src, dst, e := f.src, f.dst, f.renameErr
	f.mu.Unlock()
	if e != nil {
		return nil, e
	}
	o, e := f.Fs.NewObject(ctx, src)
	if e != nil {
		if errors.Is(e, fs.ErrorObjectNotFound) || errors.Is(e, fs.ErrorIsDir) {
			if _, de := f.Fs.List(ctx, src); de == nil {
				return nil, f.Fs.Features().DirMove(ctx, f.Fs, src, dst)
			}
		}
		return nil, e
	}
	return f.Fs.Features().Move(ctx, o, dst)
}
func (f *managedTestFS) FinishManagedRename(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "finish")
	if f.finishErr == nil {
		f.frozen = false
	}
	return f.finishErr
}
func (f *managedTestFS) AbortManagedRename(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "abort")
	f.frozen = false
	return nil
}
func (f *managedTestFS) ReplayManagedNamespace(_ context.Context, cb func(string, string) error) error {
	if f.replay != nil {
		return f.replay(cb)
	}
	return nil
}
func newManagedFS(t *testing.T, unknown bool) (*VFS, *managedTestFS, *fstest.Run) {
	t.Helper()
	r := fstest.NewRun(t)
	r.WriteObject(context.Background(), "file", "hello😀", time.Now())
	f := &managedTestFS{Fs: r.Fremote, unknown: unknown}
	opt := vfscommon.Opt
	opt.CachePollInterval = 0
	v, e := NewManaged(context.Background(), f, &opt)
	require.NoError(t, e)
	t.Cleanup(func() { cleanupVFS(t, v) })
	return v, f, r
}
func TestReaddirDoesNotMaterializeAllDocs(t *testing.T) {
	v, f, _ := newManagedFS(t, true)
	nodes, e := v.root.ReadDirAll()
	require.NoError(t, e)
	require.Len(t, nodes, 1)
	require.Equal(t, int32(0), f.materialized.Load())
}
func TestFirstStatAndOpenHaveExactProjectionSize(t *testing.T) {
	v, f, _ := newManagedFS(t, true)
	n, e := v.Stat("file")
	require.NoError(t, e)
	require.Equal(t, int64(len("hello😀")), n.Size())
	require.Equal(t, int32(1), f.materialized.Load())
	h, e := n.Open(os.O_RDONLY)
	require.NoError(t, e)
	b, e := io.ReadAll(h)
	require.NoError(t, e)
	require.Equal(t, "hello😀", string(b))
	require.NoError(t, h.Close())
}
func TestUnknownProjectionDoesNotReadAsEmpty(t *testing.T) {
	v, f, _ := newManagedFS(t, true)
	f.materializeErr = errors.New("projection parse failed")
	_, e := v.Stat("file")
	require.ErrorIs(t, e, f.materializeErr)
	_, e = v.Open("file")
	require.ErrorIs(t, e, f.materializeErr)
	require.NoError(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return f.Fs.NewObject(context.Background(), "file") }))
}
func TestManagedRenameNeverRemovesDestination(t *testing.T) {
	v, f, r := newManagedFS(t, false)
	r.WriteObject(context.Background(), "target", "old target", time.Now())
	require.NoError(t, v.Rename("file", "target"))
	require.Equal(t, int32(0), f.removed.Load())
	require.Equal(t, []string{"begin", "commit", "finish"}, f.events)
	h, e := v.Open("target")
	require.NoError(t, e)
	b, e := io.ReadAll(h)
	require.NoError(t, e)
	require.Equal(t, "hello😀", string(b))
	require.NoError(t, h.Close())
}
func TestRenameMigratesHandleAdmission(t *testing.T) {
	v, f, _ := newManagedFS(t, false)
	h, e := v.Open("file")
	require.NoError(t, e)
	require.NoError(t, v.Rename("file", "renamed"))
	require.ErrorIs(t, v.TryExternalUpdate(context.Background(), "renamed", func() (fs.Object, error) { return f.NewObject(context.Background(), "renamed") }), ErrExternalUpdateBusy)
	require.NoError(t, h.Release())
	require.NoError(t, v.TryExternalUpdate(context.Background(), "renamed", func() (fs.Object, error) { return f.NewObject(context.Background(), "renamed") }))
}
func TestManagedRenameFailureBeforeCommitAborts(t *testing.T) {
	v, f, _ := newManagedFS(t, false)
	f.renameErr = errors.New("reject")
	require.Error(t, v.Rename("file", "renamed"))
	require.Equal(t, []string{"begin", "commit", "abort"}, f.events)
	_, e := v.Stat("file")
	require.NoError(t, e)
}
func TestManagedRenameCommittedErrorNeverAborts(t *testing.T) {
	v, f, _ := newManagedFS(t, false)
	f.renameErr = &managedCommittedError{errors.New("commit acknowledged but publication failed")}
	e := v.Rename("file", "renamed")
	require.Error(t, e)
	require.Equal(t, []string{"begin", "commit"}, f.events)
}

func TestRenameFlushCannotStartSourceUpload(t *testing.T) {
	v, f, _ := newManagedFS(t, false)
	src, e := v.root.Create("temp", os.O_RDWR)
	require.NoError(t, e)
	v.root.addObject(src)
	require.Nil(t, src.getObject())
	item := v.cache.Item("temp")
	require.NoError(t, item.Open(nil))
	_, e = item.WriteAt([]byte("replacement"), 0)
	require.NoError(t, e)
	entered, leave := make(chan struct{}), make(chan struct{})
	var escaped atomic.Bool
	f.putBarrier = func() {
		f.mu.Lock()
		frozen := f.frozen
		f.events = append(f.events, "stage")
		f.mu.Unlock()
		if !frozen {
			escaped.Store(true)
		}
		close(entered)
		<-leave
	}
	done := make(chan error, 1)
	go func() { done <- v.Rename("temp", "file") }()
	select {
	case <-entered:
	case e := <-done:
		t.Fatalf("rename skipped Stage: %v", e)
	}
	_, e = v.Open("temp")
	require.ErrorIs(t, e, ErrExternalUpdateBusy)
	close(leave)
	require.NoError(t, <-done)
	require.False(t, escaped.Load())
	require.Equal(t, []string{"begin", "stage", "commit", "finish"}, f.events)
	require.NoError(t, item.Close(nil))
}
func TestManagedRenameRejectsWriterAndPostCommitFinishFailure(t *testing.T) {
	v, f, _ := newManagedFS(t, false)
	h, e := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, e)
	require.ErrorIs(t, v.Rename("file", "renamed"), ErrExternalUpdateBusy)
	require.Empty(t, f.events)
	require.NoError(t, h.Close())
	f.finishErr = errors.New("journal finish failed")
	e = v.Rename("file", "renamed")
	require.True(t, committed(e))
	require.Equal(t, []string{"begin", "commit", "finish"}, f.events)
	_, e = v.Open("renamed")
	require.ErrorIs(t, e, ErrExternalUpdateBusy)
}

func TestManagedDirectoryRenameRetainsDescendantReader(t *testing.T) {
	v, f, r := newManagedFS(t, false)
	r.WriteObject(context.Background(), "dir/child", "snapshot", time.Now())
	h, e := v.Open("dir/child")
	require.NoError(t, e)
	t.Cleanup(func() { _ = h.Release() })
	b := make([]byte, 2)
	_, e = h.Read(b)
	require.NoError(t, e)
	original := h.Node()
	original.SetSys("adapter identity")
	require.NoError(t, v.Rename("dir", "renamed"))
	n, e := v.Stat("renamed/child")
	require.NoError(t, e)
	require.Same(t, original, n)
	require.Equal(t, "adapter identity", n.Sys())
	require.ErrorIs(t, v.TryExternalUpdate(context.Background(), "renamed/child", func() (fs.Object, error) { return f.Fs.NewObject(context.Background(), "renamed/child") }), ErrExternalUpdateBusy)
	require.NoError(t, h.Close())
	require.NoError(t, v.TryExternalUpdate(context.Background(), "renamed/child", func() (fs.Object, error) { return f.Fs.NewObject(context.Background(), "renamed/child") }))
}

func TestManagedRemoveAllReservesSubtree(t *testing.T) {
	v, f, r := newManagedFS(t, false)
	r.WriteObject(context.Background(), "dir/a", "a", time.Now())
	r.WriteObject(context.Background(), "dir/b", "b", time.Now())
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.removeBarrier = func() { once.Do(func() { close(entered); <-leave }) }
	done := make(chan error, 1)
	dir, e := v.Stat("dir")
	require.NoError(t, e)
	go func() { done <- dir.RemoveAll() }()
	<-entered
	e = v.TryExternalUpdate(context.Background(), "dir/b", func() (fs.Object, error) { return f.Fs.NewObject(context.Background(), "dir/b") })
	close(leave)
	require.NoError(t, <-done)
	require.ErrorIs(t, e, ErrExternalUpdateBusy)
}
