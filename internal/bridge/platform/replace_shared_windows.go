package platform

import (
	"os"
	"path/filepath"
)

func replaceSharedFile(from, to string) error {
	// Root.Rename requests FILE_RENAME_POSIX_SEMANTICS, retaining old readers
	// while new opens see the replacement. MoveFileEx cannot replace an open
	// destination even when its readers share DELETE access.
	root, err := os.OpenRoot(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Rename(filepath.Base(from), filepath.Base(to))
}
