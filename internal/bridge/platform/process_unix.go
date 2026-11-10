//go:build !windows

package platform

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func SpawnDetached(ctx context.Context, executable string, args []string, logPath string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !filepath.IsAbs(executable) {
		return 0, os.ErrInvalid
	}
	if err := EnsurePrivateDir(filepath.Dir(logPath)); err != nil {
		return 0, err
	}
	if err := CheckNoSymlinks(logPath); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	if err = SecurePrivate(logPath, false); err != nil {
		return 0, err
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdin = nil
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}
