package overlay

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/rclone/rclone/backend/local"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

func TestFinderCreateFlushUnlinkRecreateWrite(t *testing.T) {
	f, s, _ := fixture(t)
	oldCache := fsconfig.GetCacheDir()
	cacheRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fsconfig.SetCacheDir(filepath.Join(cacheRoot, "cache")); err != nil {
		t.Fatal(err)
	}
	defer fsconfig.SetCacheDir(oldCache)
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.CachePollInterval = 0
	options.HandleCaching = 0
	managed, err := vfs.NewManaged(ctx, f, &options)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Shutdown()
	const name = "diagnostic-new.txt"
	placeholder, err := managed.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = placeholder.Flush(); err != nil {
		t.Fatal(err)
	}
	if err = placeholder.Release(); err != nil {
		t.Fatal(err)
	}
	if err = managed.DrainManagedArchives(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := s.EntryByRemote(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err = managed.Remove(name); err != nil {
		t.Fatalf("Finder placeholder cancellation failed: %v", err)
	}
	if _, err = s.EntryByRemote(ctx, name); err != store.ErrNotFound {
		t.Fatalf("placeholder still visible: %v", err)
	}
	saved, err := managed.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = saved.Write([]byte("complete Finder copy bytes")); err != nil {
		t.Fatal(err)
	}
	if err = saved.Close(); err != nil {
		t.Fatal(err)
	}
	if err = managed.DrainManagedArchives(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := managed.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = reader.Release(); err != nil {
		t.Fatal(err)
	}
	if string(content) != "complete Finder copy bytes" {
		t.Fatalf("wrong saved content: %q", content)
	}
	current, err := s.EntryByRemote(ctx, name)
	if err != nil || current.ID == original.ID {
		t.Fatalf("recreate reused cancelled identity: %+v %v", current, err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
	jobs, err := s.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].EntryID != current.ID {
		t.Fatalf("pending=%+v %v", jobs, err)
	}
	historical, err := s.ReadBlob(ctx, original.Local)
	if err != nil {
		t.Fatal("placeholder history lost", err)
	}
	historical.Close()
}
