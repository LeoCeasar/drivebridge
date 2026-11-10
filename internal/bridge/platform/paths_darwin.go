package platform

import (
	"os"
	"path/filepath"
)

func StateDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "DriveBridge", "State"), nil
}
