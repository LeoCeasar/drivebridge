package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrivateDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a", "b")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
}
