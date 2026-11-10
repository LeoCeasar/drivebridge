package vfs

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

func managedTestVFS(t *testing.T) (*VFS, fs.Object) {
	t.Helper()
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	opt.CachePollInterval = 0
	opt.HandleCaching = 0
	r, v := newTestVFSOpt(t, &opt)
	r.WriteObject(context.Background(), "file", "old", time.Now())
	o, e := r.Fremote.NewObject(context.Background(), "file")
	require.NoError(t, e)
	v.EnableManagedSync()
	return v, o
}
func TestExternalUpdateRejectsOpenReader(t *testing.T) {
	v, o := managedTestVFS(t)
	h, e := v.Open("file")
	require.NoError(t, e)
	called := false
	e = v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { called = true; return o, nil })
	require.ErrorIs(t, e, ErrExternalUpdateBusy)
	require.False(t, called)
	require.NoError(t, h.Release())
	require.NoError(t, h.Release())
	require.NoError(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return o, nil }))
}
func TestOpenCannotRaceReservedUpdate(t *testing.T) {
	v, o := managedTestVFS(t)
	_, e := v.Stat("file")
	require.NoError(t, e)
	entered, leave := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { close(entered); <-leave; return o, nil })
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("reservation ended before callback: %v", err)
	}
	_, e = v.Open("file")
	require.ErrorIs(t, e, ErrExternalUpdateBusy)
	close(leave)
	require.NoError(t, <-done)
	h, e := v.Open("file")
	require.NoError(t, e)
	require.NoError(t, h.Close())
	require.NoError(t, h.Release())
}
func TestExternalUpdateFailureReleasesReservation(t *testing.T) {
	v, _ := managedTestVFS(t)
	want := errors.New("CAS rejected")
	require.ErrorIs(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return nil, want }), want)
	h, e := v.Open("file")
	require.NoError(t, e)
	require.NoError(t, h.Close())
}
func TestNativeModeUnchanged(t *testing.T) {
	_, v := newTestVFS(t)
	require.False(t, v.ManagedSyncEnabled())
	called := false
	require.ErrorIs(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { called = true; return nil, nil }), ErrManagedSyncDisabled)
	require.False(t, called)
	h, e := v.OpenFile("native", os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, e)
	_, wrapped := h.(*managedHandle)
	require.False(t, wrapped)
	require.NoError(t, h.Close())
}

func TestManagedHandleLifetimeAndFailedOpen(t *testing.T) {
	v, o := managedTestVFS(t)
	_, e := v.OpenFile("file", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	require.Error(t, e)
	require.NoError(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return o, nil }))
	h, e := v.root.Open(os.O_RDONLY)
	require.NoError(t, e)
	require.NoError(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return o, nil }))
	require.NoError(t, h.Release())
	require.NoError(t, h.Release())
	require.NoError(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return o, nil }))
}
func TestManagedPausePermitsPublicationDrain(t *testing.T) {
	v, o := managedTestVFS(t)
	resume, e := v.PauseManagedAdmission()
	require.NoError(t, e)
	_, e = v.Open("file")
	require.ErrorIs(t, e, ErrExternalUpdateBusy)
	require.NoError(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return o, nil }))
	resume()
	resume()
	h, e := v.Open("file")
	require.NoError(t, e)
	r, w, n := v.ManagedActivity()
	require.Equal(t, 1, r)
	require.Zero(t, w+n)
	require.NoError(t, h.Release())
	r, w, n = v.ManagedActivity()
	require.Zero(t, r+w+n)
}
func TestExternalCommitCompletesAfterCancellation(t *testing.T) {
	v, o := managedTestVFS(t)
	n, e := v.Stat("file")
	require.NoError(t, e)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, v.TryExternalUpdate(ctx, "file", func() (fs.Object, error) { cancel(); return o, nil }))
	require.Same(t, n, v.root.cachedNode("file"))
	require.Equal(t, o.Size(), n.Size())
}

func TestManagedEnableRejectsSharedNativeInstance(t *testing.T) {
	opt := vfscommon.Opt
	opt.CacheMode = vfscommon.CacheModeFull
	r, native := newTestVFSOpt(t, &opt)
	second := New(context.Background(), r.Fremote, &opt)
	defer second.Shutdown()
	require.Same(t, native, second)
	native.EnableManagedSync()
	require.False(t, native.ManagedSyncEnabled())
	managed, e := NewManaged(context.Background(), r.Fremote, &opt)
	if managed != nil {
		defer managed.Shutdown()
	}
	require.Error(t, e, "two active caches must not share one filesystem identity")
}

func TestManagedReadOnlyFlushRetainsAdmission(t *testing.T) {
	v, o := managedTestVFS(t)
	h, e := v.Open("file")
	require.NoError(t, e)
	t.Cleanup(func() { _ = h.Release() })
	require.NoError(t, h.Flush())
	require.NoError(t, h.Flush())
	require.ErrorIs(t, v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) { return o, nil }), ErrExternalUpdateBusy)
	require.NoError(t, h.Release())
}
