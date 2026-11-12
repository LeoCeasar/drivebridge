package manager

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

func checkDriver() error {
	// WinFsp registers custom installation directories in the 32-bit view.
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\WinFsp`, registry.QUERY_VALUE|registry.WOW64_32KEY)
	if err == nil {
		dir, _, err := key.GetStringValue("InstallDir")
		_ = key.Close()
		if err == nil && filepath.IsAbs(dir) {
			if _, err := os.Stat(filepath.Join(dir, "bin", "winfsp-x64.dll")); err == nil {
				return nil
			}
		}
	}
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
