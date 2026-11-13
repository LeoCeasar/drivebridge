//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package integration

import (
	"os"
	"testing"

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
