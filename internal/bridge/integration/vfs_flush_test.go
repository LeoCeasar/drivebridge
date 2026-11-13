package integration

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestNewManagedFileSaveDoesNotWaitForStoreSnapshot(t *testing.T) {
	h := newHarness(t)
	f, err := h.vfs.OpenFile("new.md", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	h.io.fail.Store(true)
	defer h.io.fail.Store(false)
	if _, err = f.Write([]byte("saved locally")); err != nil {
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		t.Fatal("new-file local save entered synchronous Store snapshot", err)
	}
	if err = h.vfs.DrainManagedArchives(h.ctx); !errors.Is(err, syscallEIO()) {
		t.Fatal("background snapshot should report Store failure", err)
	}
	b := make([]byte, len("saved locally"))
	if _, err = f.ReadAt(b, 0); err != nil || string(b) != "saved locally" {
		t.Fatal(string(b), err)
	}
	saved, pending, _ := h.vfs.ManagedLocalSaveStatus()
	if !saved || pending != 1 {
		t.Fatal("local save must be distinguished from background failure", saved, pending)
	}
	h.io.fail.Store(false)
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := h.entry("new.md"); h.blob(got.Local) != "saved locally" {
		t.Fatal(got)
	}
}

func TestNewManagedFileReopensFromLocalSaveBeforeArchiveAfterRestart(t *testing.T) {
	h := newHarness(t)
	f, err := h.vfs.OpenFile("unarchived.md", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	h.io.fail.Store(true)
	defer h.io.fail.Store(false)
	if _, err = f.Write([]byte("durable unarchived body")); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = h.vfs.DrainManagedArchives(h.ctx); !errors.Is(err, syscallEIO()) {
		t.Fatal(err)
	}
	h.restart()
	b, err := h.vfs.ReadFile("unarchived.md")
	if err != nil || string(b) != "durable unarchived body" {
		t.Fatal("local saved body must be readable while archiving is unavailable", string(b), err)
	}
	h.io.fail.Store(false)
	if got := h.entry("unarchived.md"); h.blob(got.Local) != "durable unarchived body" {
		t.Fatal(got)
	}
}

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
