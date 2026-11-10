package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"golang.org/x/sys/windows"
	"os"
	"os/user"
	"strings"
	"syscall"
)

func SetStartup(ctx context.Context, s StartupSpec, enabled bool) error {
	if err := validateStartup(s); err != nil {
		return err
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	var session uint32
	if err = windows.ProcessIdToSessionId(uint32(os.Getpid()), &session); err != nil {
		return err
	}
	if session == 0 || u.Uid == "S-1-5-18" {
		return os.ErrPermission
	}
	h := sha256.Sum256([]byte(u.Uid))
	name := "DriveBridge-" + hex.EncodeToString(h[:8]) + "-" + s.ID
	if !enabled {
		return runStartup(ctx, "schtasks.exe", "/Delete", "/TN", name, "/F")
	}
	args := []string{syscall.EscapeArg(s.Executable)}
	for _, arg := range s.Args {
		args = append(args, syscall.EscapeArg(arg))
	}
	return runStartup(ctx, "schtasks.exe", "/Create", "/TN", name, "/SC", "ONLOGON", "/TR", strings.Join(args, " "), "/RU", u.Username, "/IT", "/RL", "LIMITED", "/F")
}
