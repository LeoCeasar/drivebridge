package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

func (m *Manager) waitOffline(ctx context.Context, id model.MountID) error {
	l, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		return err
	}
	ok, err := l.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPending
	}
	return l.Unlock()
}
func (m *Manager) stopForRecovery(ctx context.Context, id model.MountID, wait time.Duration) error {
	var out struct{}
	err := m.call(ctx, id, "/recovery/stop", map[string]int64{"wait_ns": int64(wait)}, &out)
	if err != nil && !errors.Is(err, ErrUnavailable) && !os.IsNotExist(err) {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait+time.Second)
	defer cancel()
	return m.waitOffline(waitCtx, id)
}
func (m *Manager) StopAll(ctx context.Context, wait time.Duration) error {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, v := range c.Mounts {
		failures = errors.Join(failures, m.Stop(ctx, v.ID, wait))
	}
	return failures
}
func (m *Manager) Wait(ctx context.Context, id model.MountID) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		state, err := m.Status(ctx, id)
		if err != nil {
			return err
		}
		if !state.Running {
			if state.Queued+state.Inflight+state.Conflicts+state.VFSPending > 0 {
				return ErrPending
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

type DoctorCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"`
}
type DoctorReport struct {
	SchemaVersion           int           `json:"schema_version"`
	Checks                  []DoctorCheck `json:"checks"`
	CLIExecutable           string        `json:"cli_executable,omitempty"`
	CLISHA256               string        `json:"cli_sha256,omitempty"`
	DocMutations            bool          `json:"doc_mutations"`
	EmptyFileUploadVerified bool          `json:"empty_file_upload_verified"`
}

func (m *Manager) Doctor(ctx context.Context, id model.ProfileID) (DoctorReport, error) {
	out := DoctorReport{SchemaVersion: 1, Checks: []DoctorCheck{}}
	var failures error
	_, _, err := selectMount()
	out.Checks = append(out.Checks, DoctorCheck{Name: "compiled_mount_engine", OK: err == nil, Code: errorCode(err)})
	failures = errors.Join(failures, err)
	err = checkDriver()
	out.Checks = append(out.Checks, DoctorCheck{Name: "mount_driver", OK: err == nil, Code: errorCode(err)})
	failures = errors.Join(failures, err)
	exe, err := ResolveCLI(m.options.CLIExecutable, m.options.Executable)
	out.Checks = append(out.Checks, DoctorCheck{Name: "official_cli", OK: err == nil, Code: errorCode(err)})
	failures = errors.Join(failures, err)
	if err == nil {
		out.CLIExecutable = exe
		grants, sum, err := trustedGrants(exe)
		if err != nil {
			failures = errors.Join(failures, err)
		}
		out.CLISHA256 = sum
		out.DocMutations = len(grants) > 0
	}
	if id != "" {
		a, err := m.Auth(ctx)
		if err == nil {
			_, err = a.Status(ctx, id)
		}
		out.Checks = append(out.Checks, DoctorCheck{Name: "verified_account", OK: err == nil, Code: errorCode(err)})
		failures = errors.Join(failures, err)
	}
	return out, failures
}
func (m *Manager) TestRemote(ctx context.Context, id model.RemoteID) (map[string]any, error) {
	r, err := m.ResolveRemote(ctx, string(id))
	if err != nil {
		return nil, err
	}
	a, err := m.Auth(ctx)
	if err != nil {
		return nil, err
	}
	runner, err := a.ManagedRunner(ctx, r.ProfileID)
	if err != nil {
		return nil, err
	}
	p, err := m.pathsMust(ctx)
	if err != nil {
		return nil, err
	}
	client, err := cloud.New(runner, cloud.Options{TempDir: filepath.Join(p, "remote-test")})
	if err != nil {
		return nil, err
	}
	items, err := client.List(ctx, r.FolderToken)
	return map[string]any{"schema_version": 1, "remote_id": id, "accessible": err == nil, "entries": len(items)}, err
}
func (m *Manager) pathsMust(ctx context.Context) (string, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return "", err
	}
	p, err := m.paths(c)
	return p.StateDir, err
}
func (m *Manager) SetAutostart(ctx context.Context, id model.MountID, enabled bool) error {
	return m.setAutostart(ctx, id, enabled, platform.SetStartup)
}
func (m *Manager) setAutostart(ctx context.Context, id model.MountID, enabled bool, startup func(context.Context, platform.StartupSpec, bool) error) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := m.readDesktopState()
	if err != nil {
		return err
	}
	if state != nil {
		_, err = m.desktopIntent(ctx, id, &enabled, true)
		return err
	}

	spec, err := m.spec(ctx, id)
	if err != nil {
		return err
	}
	args := []string{"feishu", "--bridge-config", m.options.ConfigFile}
	if m.options.CLIExecutable != "" {
		cli, err := ResolveCLI(m.options.CLIExecutable, m.options.Executable)
		if err != nil {
			return err
		}
		args = append(args, "--lark-cli", cli)
	}
	args = append(args, "_worker", "--mount-id", string(id))
	if err = startup(ctx, platform.StartupSpec{ID: string(id), Executable: m.options.Executable, Args: args}, enabled); err != nil {
		return err
	}
	return m.updateConfig(ctx, func(c *config.Config) error {
		for i := range c.Mounts {
			if c.Mounts[i].ID == spec.Mount.ID {
				c.Mounts[i].Autostart = enabled
				return nil
			}
		}
		return ErrConfig
	})
}
