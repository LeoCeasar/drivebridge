package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReplaceSharedFile atomically replaces a file while allowing existing readers
// to retain the old contents. Windows readers must have FILE_SHARE_DELETE.
// Both paths must have the same normalized parent directory; a temporary source
// should be synced before calling. Windows cannot portably sync rename metadata,
// so successful replacement does not guarantee its durability after power loss.
func ReplaceSharedFile(from, to string) error {
	if from == "" || to == "" || strings.ContainsRune(from, 0) || strings.ContainsRune(to, 0) {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrInvalid}
	}
	source, err := filepath.Abs(from)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(to)
	if err != nil {
		return err
	}
	if filepath.Dir(source) != filepath.Dir(target) {
		return fmt.Errorf("shared-file replacement requires matching parent directories: %q, %q", from, to)
	}
	return replaceSharedFile(source, target)
}
