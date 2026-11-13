package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceSharedFileRejectsDifferentDirectories(t *testing.T) {
	root := t.TempDir()
	left, right := filepath.Join(root, "left"), filepath.Join(root, "right")
	for _, dir := range []string{left, right} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	source, target := filepath.Join(left, "source"), filepath.Join(right, "target")
	decoy := filepath.Join(right, "source")
	for path, body := range map[string]string{source: "new", target: "old", decoy: "decoy"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ReplaceSharedFile(source, target); err == nil {
		t.Fatal("replacement accepted different source and target directories")
	}
	for path, want := range map[string]string{source: "new", target: "old", decoy: "decoy"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("rejected replacement changed %q: %q, %v", path, got, err)
		}
	}
}

func TestReplaceSharedFileRejectsInvalidPaths(t *testing.T) {
	for _, paths := range [][2]string{{"", "target"}, {"source", ""}, {"bad\x00source", "target"}, {"source", "bad\x00target"}} {
		if err := ReplaceSharedFile(paths[0], paths[1]); err == nil {
			t.Fatalf("replacement accepted invalid paths %q", paths)
		}
	}
}
