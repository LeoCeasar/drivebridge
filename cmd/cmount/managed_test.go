//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package cmount

import (
	"context"
	"errors"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
	"github.com/winfsp/cgofuse/fuse"
	"syscall"
	"testing"
)

type managedFailureHandle struct {
	vfs.Handle
	calls int
}

func (h *managedFailureHandle) Sync() error { h.calls++; return errors.Join(syscall.ENOSPC) }
func TestManagedFsyncForwardsFailure(t *testing.T) {
	backend, e := fs.NewFs(context.Background(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	v, e := vfs.NewManaged(context.Background(), backend, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = v.CleanUp(); v.Shutdown() })
	h := &managedFailureHandle{}
	adapter := &FS{VFS: v, handles: []vfs.Handle{h}}
	if got := adapter.Fsync("file", false, 0); got != -fuse.ENOSPC || h.calls != 1 {
		t.Fatal(got, h.calls)
	}
	native := vfs.New(context.Background(), backend, nil)
	defer native.Shutdown()
	adapter.VFS = native
	if got := adapter.Fsync("file", false, 0); got != 0 || h.calls != 1 {
		t.Fatal("native no-op changed", got, h.calls)
	}
}
