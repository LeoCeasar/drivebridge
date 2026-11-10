package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

func StateDir() (string, error) {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("XDG_STATE_HOME must be absolute")
		}
		return filepath.Join(root, "drivebridge"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "drivebridge"), nil
}
