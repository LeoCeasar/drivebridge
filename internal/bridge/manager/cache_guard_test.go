package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/file"
)

func TestOfflineCacheManifestAcceptsLongPathRoots(t *testing.T) {
	root := t.TempDir()
	o := ownership{Root: root, VFSRoot: filepath.Join(root, "vfs")}
	o.Scope.MountID = model.MountID("fixture")
	manifest := cacheManifest{SchemaVersion: 1, MountID: o.Scope.MountID,
		DataRoot: file.UNCPath(filepath.Join(o.VFSRoot, "data")),
		MetaRoot: file.UNCPath(filepath.Join(o.VFSRoot, "meta"))}
	path := filepath.Join(root, "cache-manifest.json")
	if err := atomicJSON(path, manifest); err != nil {
		t.Fatal(err)
	}
	if pending, err := offlineCachePending(o); pending || err != nil {
		t.Fatalf("empty managed cache: pending=%v err=%v", pending, err)
	}
	manifest.DataRoot = file.UNCPath(filepath.Join(root, "outside"))
	if err := atomicJSON(path, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := offlineCachePending(o); err != ErrConfig {
		t.Fatalf("outside managed cache: %v", err)
	}
}

func TestMissingCacheManifestWithOnlyEmptyDirectoriesIsNotPending(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := ownership{Root: root, VFSRoot: filepath.Join(root, "vfs")}
	if err := os.MkdirAll(filepath.Join(o.VFSRoot, "vfsMeta", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if pending, err := offlineCachePending(o); err != nil || pending {
		t.Fatalf("failed mount left no cache bytes: pending=%v err=%v", pending, err)
	}
	file := filepath.Join(o.VFSRoot, "vfsMeta", "empty", "orphan")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if pending, err := offlineCachePending(o); err != nil || !pending {
		t.Fatalf("even zero-byte orphan must remain protected: pending=%v err=%v", pending, err)
	}
}

func TestMissingCacheManifestDoesNotFollowNestedSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := ownership{Root: root, VFSRoot: filepath.Join(root, "vfs")}
	if err := os.MkdirAll(o.VFSRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(o.VFSRoot, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if pending, err := offlineCachePending(o); err == nil && !pending {
		t.Fatal("unknown symbolic link must prevent replacement")
	}
}
