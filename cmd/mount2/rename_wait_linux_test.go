//go:build linux

package mount2

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
)

func TestRenameWaitPreservesFlushedLiveHandle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("body"), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := fs.NewFs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := vfs.NewManaged(context.Background(), backend, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = managed.CleanUp(); managed.Shutdown() })
	dir, err := managed.Root()
	if err != nil {
		t.Fatal(err)
	}
	h, err := managed.OpenFile("file", os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	adapter := newFileHandle(h, &FS{VFS: managed})
	if errno := adapter.Flush(context.Background()); errno != 0 {
		t.Fatal(errno)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	node := &Node{node: dir, fsys: &FS{VFS: managed}}
	if errno := node.Rename(ctx, "file", node, "renamed", 0); errno != syscall.EBUSY {
		t.Fatalf("held duplicate must block namespace mutation with EBUSY: %v", errno)
	}
	if _, errno := adapter.Write(context.Background(), []byte("still live"), 0); errno != 0 {
		t.Fatalf("Flush must preserve the surviving duplicate: %v", errno)
	}
	if _, err := os.Stat(filepath.Join(root, "renamed")); !os.IsNotExist(err) {
		t.Fatalf("readiness mutated namespace: %v", err)
	}
}
