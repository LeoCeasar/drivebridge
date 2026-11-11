package manager

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/rclone/rclone/internal/bridge/platform"
)

func prepareAccountMountpoint(path string) error {
	return prepareMountpointForOS(path, runtime.GOOS, true)
}

// prepareMountpointForOS leaves Windows directory targets absent for WinFsp.
// private is used only for newly configured automatic account targets. Runtime
// validation must not change the permissions of an existing user-selected parent.
// Existing targets are never removed or relocated, even when empty on Windows.
func prepareMountpointForOS(path, goos string, private bool) error {
	drive := goos == "windows" && isDrive(path)
	if !filepath.IsAbs(path) && !drive {
		return ErrConfig
	}
	if !drive {
		if err := platform.CheckNoSymlinks(path); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return ErrConfig
		}
		if goos == "windows" {
			return ErrPending
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return ErrPending
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if drive {
		return nil
	}
	target := path
	if goos == "windows" {
		target = filepath.Dir(path)
	}
	if private {
		return platform.EnsurePrivateDir(target)
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	return platform.CheckNoSymlinks(target)
}
