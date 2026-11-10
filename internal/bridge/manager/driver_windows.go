package manager

import (
	"os"
	"path/filepath"
)

func checkDriver() error {
	for _, root := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles")} {
		if root == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "WinFsp", "bin", "winfsp-x64.dll")); err == nil {
			return nil
		}
	}
	return ErrDriver
}
