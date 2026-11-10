// Package platform provides current-user path and private-file primitives.
package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnsurePrivateDir creates a private directory tree without following symlinks.
// Existing ancestors are inspected, but only the requested directory is secured.
func EnsurePrivateDir(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("private directory must be absolute: %q", path)
	}
	if err := CheckNoSymlinks(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		parent := filepath.Dir(path)
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			if err := EnsurePrivateDir(parent); err != nil {
				return err
			}
		}
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a plain directory: %s", path)
	}
	return SecurePrivate(path, true)
}

// CheckNoSymlinks rejects existing symlinks in a private path, including its leaf.
// Windows additionally rejects every reparse point, including junction ancestors;
// this conservatively excludes cloud placeholders and other reparse-backed storage.
// It is a preflight check; callers must not treat it as an adversarial TOCTOU barrier.
func CheckNoSymlinks(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			alias, err := privatePathAlias(p, info)
			if err != nil {
				return err
			}
			if alias {
				return fmt.Errorf("symlink or reparse point in private path: %s", p)
			}
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}
