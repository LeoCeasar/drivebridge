package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func systemdArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
}
func SetStartup(ctx context.Context, s StartupSpec, enabled bool) error {
	if err := validateStartup(s); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return os.ErrPermission
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "systemd", "user")
	if err = EnsurePrivateDir(dir); err != nil {
		return err
	}
	name := "drivebridge-" + s.ID + ".service"
	file := filepath.Join(dir, name)
	if enabled {
		if err = CheckNoSymlinks(file); err != nil {
			return err
		}
		args := []string{systemdArg(s.Executable)}
		for _, arg := range s.Args {
			args = append(args, systemdArg(arg))
		}
		data := "[Unit]\nDescription=DriveBridge personal Feishu mount\nAfter=network-online.target\n[Service]\nType=simple\nExecStart=" + strings.Join(args, " ") + "\nRestart=no\n[Install]\nWantedBy=default.target\n"
		if err = os.WriteFile(file, []byte(data), 0600); err != nil {
			return err
		}
		if err = SecurePrivate(file, false); err != nil {
			return err
		}
		if err = runStartup(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		return runStartup(ctx, "systemctl", "--user", "enable", name)
	}
	if err = runStartup(ctx, "systemctl", "--user", "disable", name); err != nil {
		return err
	}
	if err = os.Remove(file); err != nil && !os.IsNotExist(err) {
		return err
	}
	return runStartup(ctx, "systemctl", "--user", "daemon-reload")
}
