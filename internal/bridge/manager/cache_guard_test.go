package manager

import (
	"os"
	"path/filepath"
	"testing"
)

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
