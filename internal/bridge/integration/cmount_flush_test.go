//go:build cmount && darwin && cgo

package integration

import (
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/cmount"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/winfsp/cgofuse/fuse"
)

func TestManagedCmountCopyCallbacksDoNotDeleteAcceptedBytes(t *testing.T) {
	for _, name := range []string{"sync-check.md", "sync-check.txt"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.vfs.Opt.WriteBack = fs.Duration(2 * time.Second)
			adapter := cmount.NewFS(h.vfs, &mountlib.Options{NoAppleXattr: true, ExtraOptions: []string{"backend=fskit"}})
			code, dirHandle := adapter.Opendir("/")
			if code != 0 {
				t.Fatal(code)
			}
			defer adapter.Releasedir("/", dirHandle)
			path := "/" + name
			code, handle := adapter.Create(path, fuse.O_CREAT|fuse.O_EXCL|fuse.O_RDWR, 0600)
			if code != 0 {
				t.Fatal(code)
			}
			var stat fuse.Stat_t
			if code = adapter.Getattr(path, &stat, handle); code != 0 || stat.Size != 0 {
				t.Fatal(code, stat)
			}
			if code = adapter.Flush(path, handle); code != 0 {
				t.Fatal(code)
			}
			body := []byte("full synthetic Finder copy body\nversion=local-1\n")
			if count := adapter.Write(path, body, 0, handle); count != len(body) {
				t.Fatal(count)
			}
			if code = adapter.Fsync(path, false, handle); code != 0 {
				t.Fatal(code)
			}
			timestamp := fuse.NewTimespec(time.Now().Add(-time.Hour))
			for _, code := range []int{
				adapter.Setxattr(path, "com.apple.FinderInfo", []byte("metadata"), 0),
				adapter.Chmod(path, 0600), adapter.Chown(path, 501, 20),
				adapter.Utimens(path, []fuse.Timespec{timestamp, timestamp}),
				adapter.Flush(path, handle), adapter.Release(path, handle),
			} {
				if code != 0 {
					t.Fatal(code)
				}
			}
			if code = adapter.Getattr(path, &stat, ^uint64(0)); code != 0 || stat.Size != int64(len(body)) {
				t.Fatal(code, stat)
			}
			entry := h.entry(name)
			conflicts, err := h.store.Conflicts(h.ctx)
			if err != nil || len(conflicts) != 0 || h.blob(entry.Local) != string(body) {
				t.Fatal(entry, conflicts, err)
			}
		})
	}
}
