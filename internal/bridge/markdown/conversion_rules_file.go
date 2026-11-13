package markdown

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rclone/rclone/internal/bridge/platform"
)

// EnsureConversionRulesFile publishes an editable default template beside the
// actual application config. Existing user files, including invalid ones, are
// never replaced or rewritten. Call after configuration initialization succeeds.
func EnsureConversionRulesFile(configFile string) (err error) {
	path := ConversionRulesPath(configFile)
	defer func() {
		if err != nil {
			err = fmt.Errorf("conversion rules %s: %w", path, err)
		}
	}()
	if !filepath.IsAbs(configFile) {
		return fmt.Errorf("absolute config file path required")
	}
	if err = platform.CheckNoSymlinks(configFile); err != nil {
		return err
	}
	if exists, checkErr := existingConversionRulesFile(path); checkErr != nil || exists {
		return checkErr
	}
	dir := filepath.Dir(path)
	if err = platform.EnsurePrivateDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".conversion-rules-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(temporary) }()
	if err = platform.SecurePrivate(temporary, false); err != nil {
		return err
	}
	if _, err = f.Write(DefaultConversionRulesJSON()); err != nil {
		return err
	}
	if err = platform.SyncFile(f); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = platform.CheckNoSymlinks(path); err != nil {
		return err
	}
	// A hard link publishes the complete synced inode without replacing a file
	// another initializer or user may have created while the template was written.
	if err = os.Link(temporary, path); err != nil {
		if os.IsExist(err) {
			if exists, checkErr := existingConversionRulesFile(path); checkErr != nil || exists {
				return checkErr
			}
		}
		return err
	}
	if err = os.Remove(temporary); err != nil {
		return err
	}
	return platform.SyncDir(dir)
}

func existingConversionRulesFile(path string) (bool, error) {
	if err := platform.CheckNoSymlinks(path); err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("expected a regular user rules file")
	}
	return true, nil
}
