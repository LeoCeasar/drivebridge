//go:build cmount && darwin && cgo

package integration

import (
	"testing"

	"github.com/rclone/rclone/cmd/cmount"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/winfsp/cgofuse/fuse"
)

// The callbacks sit below FSKit's own inode cache: this verifies that a longer
// canonical projection reaches the adapter intact, without a kernel mount.
func TestManagedCmountMarkdownPublicationUpdatesSizeAndContent(t *testing.T) {
	h := newHarness(t)
	const name = "sync-check.md"
	const path = "/" + name
	adapter := cmount.NewFS(h.vfs, &mountlib.Options{NoAppleXattr: true, ExtraOptions: []string{"backend=fskit"}})
	code, writer := adapter.Create(path, fuse.O_CREAT|fuse.O_EXCL|fuse.O_RDWR, 0600)
	if code != 0 {
		t.Fatal(code)
	}
	if n := adapter.Write(path, []byte(canonicalSizeInput), 0, writer); n != len(canonicalSizeInput) {
		t.Fatal(n)
	}
	if code = adapter.Flush(path, writer); code != 0 {
		t.Fatal(code)
	}
	if code = adapter.Release(path, writer); code != 0 {
		t.Fatal(code)
	}
	var stat fuse.Stat_t
	if code = adapter.Getattr(path, &stat, ^uint64(0)); code != 0 || stat.Size != int64(len(canonicalSizeInput)) {
		t.Fatal(code, stat)
	}
	if err := h.engine.SyncEntry(h.ctx, h.entry(name).ID); err != nil {
		t.Fatal(err)
	}
	canonical := h.blob(h.entry(name).Visible)
	if len(canonical) <= len(canonicalSizeInput) {
		t.Fatalf("canonical fixture must grow: raw=%d canonical=%d", len(canonicalSizeInput), len(canonical))
	}
	if code = adapter.Getattr(path, &stat, ^uint64(0)); code != 0 || stat.Size != int64(len(canonical)) {
		t.Fatal(code, stat)
	}
	var reader fuse.FileInfo_t
	if code = adapter.OpenEx(path, &reader); code != 0 {
		t.Fatal(code)
	}
	defer adapter.Release(path, reader.Fh)
	buffer := make([]byte, len(canonical)+512)
	if n := adapter.Read(path, buffer, 0, reader.Fh); n != len(canonical) || string(buffer[:max(0, n)]) != canonical {
		t.Fatalf("callback read=%d want=%d canonical=%q", n, len(canonical), canonical)
	}
	if n := adapter.Read(path, buffer, int64(len(canonicalSizeInput)), reader.Fh); n != len(canonical)-len(canonicalSizeInput) || string(buffer[:max(0, n)]) != canonical[len(canonicalSizeInput):] {
		t.Fatalf("tail callback read=%d want=%q", n, canonical[len(canonicalSizeInput):])
	}
	t.Logf("raw=%d canonical=%d getattr=%d; callback returns complete body and tail", len(canonicalSizeInput), len(canonical), stat.Size)
}
