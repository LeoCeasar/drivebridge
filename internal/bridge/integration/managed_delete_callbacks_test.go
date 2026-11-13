//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package integration

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/overlay"
	"github.com/rclone/rclone/vfs"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/cmount"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/stretchr/testify/require"
	"github.com/winfsp/cgofuse/fuse"
)

// WinFsp calls unlink during Cleanup, before Close releases its own read-only
// deletion handle. This is a callback-contract regression, not a native mount.
func TestManagedWinFspDeleteKeepsReaderGeneration(t *testing.T) {
	h := newHarness(t)
	require.Equal(t, "\x00\xffinitial bytes", h.read("data.bin"))
	a := cmount.NewFS(h.vfs, &mountlib.Opt)
	code, handle := a.Open("/data.bin", os.O_RDONLY)
	require.Zero(t, code)
	defer a.Release("/data.bin", handle)
	require.Zero(t, a.Unlink("/data.bin"), "cleanup must not reject its own reader")
	_, err := h.store.EntryByRemote(h.ctx, "data.bin")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = h.vfs.Open("data.bin")
	require.Error(t, err, "a new open must not resurrect the deleted path")
	buf := make([]byte, 64)
	n := a.Read("/data.bin", buf, 0, handle)
	require.Equal(t, "\x00\xffinitial bytes", string(buf[:n]))
	h.save("data.bin", "different new file")
	n = a.Read("/data.bin", buf, 0, handle)
	require.Equal(t, "\x00\xffinitial bytes", string(buf[:n]), "old reader must not bind to a recreated file")
	require.Equal(t, "different new file", h.read("data.bin"))
}

func TestManagedWinFspDirectoryDeletePrecedesRelease(t *testing.T) {
	h := newHarness(t)
	dir, err := h.vfs.Root()
	require.NoError(t, err)
	_, err = dir.Mkdir("empty")
	require.NoError(t, err)
	a := cmount.NewFS(h.vfs, &mountlib.Opt)
	code, handle := a.Opendir("/empty")
	require.Zero(t, code)
	defer a.Releasedir("/empty", handle)
	require.Zero(t, a.Rmdir("/empty"), "cleanup must not reject its directory snapshot")
	_, err = h.store.EntryByRemote(h.ctx, "empty")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestManagedDeleteRetainsActiveWriterProtection(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	a := cmount.NewFS(h.vfs, &mountlib.Opt)
	code, handle := a.Open("/data.bin", os.O_RDWR)
	require.Zero(t, code)
	defer a.Release("/data.bin", handle)
	require.Equal(t, -fuse.EBUSY, a.Unlink("/data.bin"))
	_, err := h.store.EntryByRemote(h.ctx, "data.bin")
	require.NoError(t, err)
}

func TestManagedDeleteAccessRejectsActiveWriterBeforeCleanup(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	a := cmount.NewFS(h.vfs, &mountlib.Opt)
	code, reader := a.Open("/data.bin", os.O_RDONLY)
	require.Zero(t, code)
	require.Zero(t, a.Access("/data.bin", fuse.DELETE_OK))
	require.Zero(t, a.Release("/data.bin", reader))
	code, writer := a.Open("/data.bin", os.O_RDWR)
	require.Zero(t, code)
	defer a.Release("/data.bin", writer)
	require.Equal(t, -fuse.EBUSY, a.Access("/data.bin", fuse.DELETE_OK), "WinFsp must receive the error before its void Cleanup callback")
	_, err := h.store.EntryByRemote(h.ctx, "data.bin")
	require.NoError(t, err)
}

func TestManagedDeleteAccessProtectsReadonlyAssetsWithoutMutation(t *testing.T) {
	h := newHarness(t)
	a := cmount.NewFS(h.vfs, &mountlib.Opt)
	require.Equal(t, -fuse.EPERM, a.Access("/.drivebridge-assets/image.png", fuse.DELETE_OK))
	require.Zero(t, a.Access("/data.bin", fuse.DELETE_OK))
	actions, err := h.store.NamespaceActions(h.ctx)
	require.NoError(t, err)
	require.Empty(t, actions, "delete permission checks must not prepare a real deletion")
	_, _, reservations := h.vfs.ManagedActivity()
	require.Zero(t, reservations)
}

// Admission precedes materialization/cache-handle construction. An unbound
// reader cannot survive unlink yet: it could recreate the deleted cache path.
type managedOpenGate struct {
	*overlay.Fs
	enabled          atomic.Bool
	entered, release chan struct{}
}

func (g *managedOpenGate) TouchManagedAccess(ctx context.Context, name string) error {
	err := g.Fs.TouchManagedAccess(ctx, name)
	if g.enabled.Swap(false) {
		close(g.entered)
		<-g.release
	}
	return err
}
func TestManagedDeleteWaitsForReaderConstruction(t *testing.T) {
	h := newHarness(t)
	options := h.vfs.Opt
	h.vfs.Shutdown()
	g := &managedOpenGate{Fs: h.overlay, entered: make(chan struct{}), release: make(chan struct{})}
	var err error
	h.vfs, err = vfs.NewManaged(h.ctx, g, &options)
	require.NoError(t, err)
	require.Equal(t, "\x00\xffinitial bytes", h.read("data.bin"))
	var once sync.Once
	resume := func() { once.Do(func() { close(g.release) }) }
	defer resume()
	type result struct {
		handle vfs.Handle
		err    error
	}
	done := make(chan result, 1)
	g.enabled.Store(true)
	go func() { f, e := h.vfs.Open("data.bin"); done <- result{f, e} }()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("open did not reach construction gate")
	}
	a := cmount.NewFS(h.vfs, &mountlib.Opt)
	require.Equal(t, -fuse.EBUSY, a.Access("/data.bin", fuse.DELETE_OK))
	require.Equal(t, -fuse.EBUSY, a.Unlink("/data.bin"), "pending reader has no pinned generation yet")
	resume()
	var opened result
	select {
	case opened = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("open did not resume")
	}
	require.NoError(t, opened.err)
	defer opened.handle.Close()
	require.Zero(t, a.Unlink("/data.bin"), "a constructed reader may survive unlink")
	body, err := io.ReadAll(opened.handle)
	require.NoError(t, err)
	require.Equal(t, "\x00\xffinitial bytes", string(body))
	_, err = h.vfs.Stat("data.bin")
	require.Error(t, err, "a surviving reader must never resurrect the removed path")
}
