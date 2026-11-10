//go:build linux || (darwin && amd64)

package mount2

import (
	"context"
	"errors"
	"github.com/hanwen/go-fuse/v2/fuse"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type failingManagedHandle struct{ vfs.Handle }

func (h failingManagedHandle) Flush() error { return errors.Join(syscall.ENOSPC) }
func (h failingManagedHandle) Sync() error  { return errors.Join(syscall.EIO) }
func TestManagedErrorTranslation(t *testing.T) {
	if got := translateManagedError(errors.Join(syscall.ENOSPC)); got != syscall.ENOSPC {
		t.Fatal(got)
	}
	if got := translateManagedError(vfs.ErrExternalUpdateBusy); got != syscall.EBUSY {
		t.Fatal(got)
	}
	f := &FileHandle{h: failingManagedHandle{}}
	if got := f.Flush(context.Background()); got != syscall.EIO {
		t.Fatal("native fallback changed", got)
	}
	if got := f.Fsync(context.Background(), 0); got != syscall.EIO {
		t.Fatal(got)
	}
}

func TestManagedFlushFsyncForward(t *testing.T) {
	backend, e := fs.NewFs(context.Background(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	v, e := vfs.NewManaged(context.Background(), backend, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = v.CleanUp(); v.Shutdown() })
	f := &FileHandle{h: failingManagedHandle{}, fsys: &FS{VFS: v}}
	if got := f.Flush(context.Background()); got != syscall.ENOSPC {
		t.Fatal(got)
	}
	if got := f.Fsync(context.Background(), 0); got != syscall.EIO {
		t.Fatal(got)
	}
}

type unknownManagedObject struct{ fs.Object }

func (unknownManagedObject) Size() int64 { return -1 }

type unknownManagedFS struct{ fs.Fs }

func (f unknownManagedFS) List(ctx context.Context, p string) (fs.DirEntries, error) {
	a, e := f.Fs.List(ctx, p)
	for i, n := range a {
		if o, ok := n.(fs.Object); ok {
			a[i] = unknownManagedObject{o}
		}
	}
	return a, e
}
func (f unknownManagedFS) MaterializeManaged(context.Context, string) (fs.Object, error) {
	return nil, syscall.EIO
}
func TestManagedExistingNodeGetattrPropagatesMaterializationFailure(t *testing.T) {
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "file"), []byte("body"), 0600); e != nil {
		t.Fatal(e)
	}
	backend, e := fs.NewFs(context.Background(), root)
	if e != nil {
		t.Fatal(e)
	}
	v, e := vfs.NewManaged(context.Background(), unknownManagedFS{backend}, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = v.CleanUp(); v.Shutdown() })
	dir, e := v.Root()
	if e != nil {
		t.Fatal(e)
	}
	nodes, e := dir.ReadDirAll()
	if e != nil {
		t.Fatal(e)
	}
	node := &Node{node: nodes[0], fsys: &FS{VFS: v}}
	if got := node.Getattr(context.Background(), nil, &fuse.AttrOut{}); got != syscall.EIO {
		t.Fatal(got)
	}
}
