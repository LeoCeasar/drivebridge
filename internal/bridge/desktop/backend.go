package desktop

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/desktopstartup"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

type managerBackend struct {
	manager *manager.Manager
	open    func(context.Context) error
	startup desktopstartup.Spec
}

func (b *managerBackend) Mounts(ctx context.Context) ([]Mount, error) {
	mounts, err := b.manager.DesktopMounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, Mount{ID: m.Mount.ID, Name: m.Mount.Name, Mountpoint: m.Mount.Mountpoint, Desired: m.Desired})
	}
	return out, nil
}
func (b *managerBackend) Status(ctx context.Context, id model.MountID) (model.MountStatus, error) {
	return b.manager.Status(ctx, id)
}
func (b *managerBackend) Start(ctx context.Context, id model.MountID, automatic bool) error {
	if automatic {
		_, err := b.manager.StartDesktopMount(ctx, id)
		return err
	}
	_, err := b.manager.Start(ctx, id, true)
	return err
}
func (b *managerBackend) Stop(ctx context.Context, id model.MountID) error {
	return b.manager.Stop(ctx, id, 60*time.Second)
}
func (b *managerBackend) Retry(ctx context.Context, id model.MountID) error {
	s, err := b.manager.Status(ctx, id)
	if err != nil {
		return err
	}
	if !s.Running {
		if _, err = b.manager.Start(ctx, id, true); err != nil {
			return err
		}
	}
	return b.manager.Retry(ctx, id)
}
func (b *managerBackend) OpenManagement(ctx context.Context) error { return b.open(ctx) }
func (b *managerBackend) OpenFolder(ctx context.Context, id model.MountID) error {
	m, err := b.manager.ResolveMount(ctx, string(id))
	if err != nil {
		return err
	}
	return openFolder(ctx, m.Mountpoint)
}
func (b *managerBackend) SetAutostart(ctx context.Context, enabled bool) error {
	return desktopstartup.Set(ctx, b.startup, enabled)
}
func (b *managerBackend) Autostart(ctx context.Context) (bool, error) {
	return desktopstartup.Enabled(ctx, b.startup)
}
func openFolder(ctx context.Context, path string) error {
	if runtime.GOOS == "windows" && len(path) == 2 && path[1] == ':' {
		path += `\`
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("invalid configured folder")
	}
	if !platform.DesktopAvailable() {
		return platform.ErrNoDesktop
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", "--", path)
	case "windows":
		command = exec.CommandContext(ctx, "explorer.exe", path)
	default:
		command = exec.CommandContext(ctx, "xdg-open", path)
	}
	return command.Run()
}

func (b *managerBackend) Accounts(ctx context.Context) ([]AccountView, error) {
	views, err := b.manager.DiscoveryViews(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := b.manager.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	names := map[model.ProfileID]string{}
	for _, profile := range cfg.Profiles {
		names[profile.ID] = profile.Name
	}
	out := make([]AccountView, 0, len(views))
	for _, view := range views {
		canRescan := view.Configured && view.Enabled
		state := view.State
		switch state {
		case "needs_runtime", "needs_login", "login_pending", "scanning", "needs_browser", "needs_extension", "browser_disconnected", "unconfigured", "disabled", "unsupported":
			canRescan = false
		}
		out = append(out, AccountView{ID: view.ProfileID, Name: names[view.ProfileID], State: state, Configured: view.Configured, Enabled: view.Enabled, CanRescan: canRescan, LastScan: view.LastScan})
	}
	return out, nil
}
func (b *managerBackend) RescanAccount(ctx context.Context, id model.ProfileID) error {
	_, err := b.manager.RescanDiscovery(ctx, id)
	return err
}
