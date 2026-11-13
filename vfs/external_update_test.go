package vfs

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
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

// Immutable objects model the managed overlay: publication changes a mapping,
// while old objects continue to open their original complete body.
type publicationTestFS struct {
	fs.Fs
	mu      sync.RWMutex
	current fs.Object
}

func (f *publicationTestFS) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	if remote != "file" {
		return f.Fs.NewObject(ctx, remote)
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.current, nil
}
func (f *publicationTestFS) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if dir != "" {
		return f.Fs.List(ctx, dir)
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return fs.DirEntries{f.current}, nil
}

func TestReadsDuringReservedUpdateKeepTheirVersion(t *testing.T) {
	v, _ := managedTestVFS(t)
	backing := &publicationTestFS{Fs: v.f, current: object.NewMemoryObject("file", time.Now(), []byte("old"))}
	v.f = backing
	v.root.f = backing
	warm, err := v.Open("file")
	require.NoError(t, err)
	body, err := io.ReadAll(warm)
	require.NoError(t, err)
	require.Equal(t, "old", string(body))
	require.NoError(t, warm.Close())
	next := object.NewMemoryObject("file", time.Now().Add(time.Second), []byte("complete-new-version"))
	entered, leave := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) {
			close(entered)
			<-leave
			backing.mu.Lock()
			backing.current = next
			backing.mu.Unlock()
			return next, nil
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("callback not reached: %v", err)
	}
	during, err := v.Open("file")
	require.NoError(t, err)
	duringBody, err := io.ReadAll(during)
	require.NoError(t, err)
	require.Equal(t, "old", string(duringBody))
	require.NoError(t, during.Close())
	old, err := v.Open("file")
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Close() })
	root, err := v.root.Open(os.O_RDONLY)
	require.NoError(t, err)
	writerDone := make(chan struct {
		h   Handle
		err error
	}, 1)
	go func() {
		h, err := v.OpenFile("file", os.O_WRONLY, 0600)
		writerDone <- struct {
			h   Handle
			err error
		}{h, err}
	}()
	select {
	case result := <-writerDone:
		if result.h != nil {
			_ = result.h.Close()
		}
		t.Fatalf("writer completed before local publication: %v", result.err)
	case <-time.After(20 * time.Millisecond):
	}
	close(leave)
	require.NoError(t, <-done)
	writer := <-writerDone
	require.NoError(t, writer.err)
	require.NoError(t, writer.h.Close())
	// Force cache pressure before the lazy reader's first Read: its pin must
	// protect the detached old generation from quota eviction.
	oldItem := old.(*managedHandle).Handle.(*RWFileHandle).item
	removed, _ := oldItem.RemoveNotInUse(0, false)
	require.False(t, removed)
	// The old handle has not read yet: its lazy open must retain the old object.
	body, err = io.ReadAll(old)
	require.NoError(t, err)
	require.Equal(t, "old", string(body))
	require.Equal(t, next.Size(), v.root.cachedNode("file").Size())
	newer, err := v.Open("file")
	require.NoError(t, err)
	t.Cleanup(func() { _ = newer.Close() })
	body, err = io.ReadAll(newer)
	require.NoError(t, err)
	require.Equal(t, "complete-new-version", string(body))
	require.NoError(t, old.Close())
	require.NoError(t, newer.Close())
	require.NoError(t, root.Close())
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

func TestExternalPublicationPreservesDirtyWrites(t *testing.T) {
	v, _ := managedTestVFS(t)
	h, err := v.OpenFile("file", os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = h.WriteAt([]byte("user-save"), 0)
	require.NoError(t, err)

	called := false
	err = v.TryExternalUpdate(context.Background(), "file", func() (fs.Object, error) {
		called = true
		return object.NewMemoryObject("file", time.Now(), []byte("remote")), nil
	})
	require.ErrorIs(t, err, ErrExternalUpdateBusy)
	require.False(t, called)
	require.NoError(t, h.Close())
	read, err := v.Open("file")
	require.NoError(t, err)
	body, err := io.ReadAll(read)
	require.NoError(t, err)
	require.Equal(t, "user-save", string(body))
	require.NoError(t, read.Close())
}
