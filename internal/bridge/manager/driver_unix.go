//go:build !windows

package manager

import (
	"os"
	"os/exec"
	"runtime"
)

func checkDriver() error {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/Library/Filesystems/macfuse.fs"); err != nil {
			return ErrDriver
		}
		return nil
	}
	if runtime.GOOS == "linux" {
		f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
		if err != nil {
			return ErrDriver
		}
		_ = f.Close()
		if _, err = exec.LookPath("fusermount3"); err != nil {
			return ErrDriver
		}
		return nil
	}
	return ErrDriver
}
