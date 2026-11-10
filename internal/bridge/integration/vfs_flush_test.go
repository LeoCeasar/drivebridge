package integration

import (
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

// Exercise the native adapter's create/empty-flush/write/repeated-flush/release
// sequence against the real durable overlay. No kernel mount or account is used.
func TestManagedCopyFlushReleaseDoesNotDeleteAcceptedOverlayBytes(t *testing.T) {
	for _, name := range []string{"sync-check.md", "sync-check.txt"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.vfs.Opt.WriteBack = fs.Duration(2 * time.Second)
			root, err := h.vfs.Root()
			if err != nil {
				t.Fatal(err)
			}
			parentReader, err := root.Open(os.O_RDONLY)
			if err != nil {
				t.Fatal(err)
			}
			defer parentReader.Close()
			file, err := root.Create(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
			if err != nil {
				t.Fatal(err)
			}
			writer, err := file.Open(os.O_CREATE | os.O_EXCL | os.O_WRONLY)
			if err != nil {
				t.Fatal(err)
			}
			if err = writer.Flush(); err != nil {
				t.Fatal(err)
			}
			empty := h.entry(name)
			if empty.Generation != 1 || h.blob(empty.Local) != "" {
				t.Fatal(empty)
			}
			body := "full synthetic Finder copy body\nversion=local-1\n"
			if _, err = writer.WriteAt([]byte(body), 0); err != nil {
				t.Fatal(err)
			}
			if err = file.SetModTime(time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			for _, flush := range []func() error{writer.Sync, writer.Flush, writer.Flush, writer.Close, writer.Release} {
				if err = flush(); err != nil {
					t.Fatal(err)
				}
				entry := h.entry(name)
				if entry.Status == model.StatusConflict || h.blob(entry.Local) != body {
					t.Fatal("accepted bytes changed or entered conflict", entry)
				}
				conflicts, err := h.store.Conflicts(h.ctx)
				if err != nil || len(conflicts) != 0 {
					t.Fatal(conflicts, err)
				}
			}
			if h.vfs.ManagedCachePending() {
				t.Fatal("flush/release left unaccepted VFS bytes")
			}
		})
	}
}
