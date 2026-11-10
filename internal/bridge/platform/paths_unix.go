//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
)

func SecurePrivate(path string, directory bool) error {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return os.Chmod(path, mode)
}

// ReplaceFile atomically replaces a file within a filesystem.
func ReplaceFile(from, to string) error { return os.Rename(from, to) }

// SyncDir persists a directory entry after an atomic rename.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ValidMountpoint checks syntax only; the mount manager checks runtime availability.
func ValidMountpoint(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsRune(path, 0) && filepath.Clean(path) != "/"
}
