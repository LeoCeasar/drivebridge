package manager

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/overlay"
	"github.com/rclone/rclone/internal/bridge/ownedlogin"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/lib/larkcli"
)

func (m *Manager) discoveryDir(ctx context.Context, id model.ProfileID) (string, error) {
	if !validID(string(id)) {
		return "", ErrConfig
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return "", err
	}
	p, err := m.paths(c)
	if err != nil {
		return "", err
	}
	return filepath.Join(p.StateDir, "discovery", string(id)), nil
}
func readDiscoveryJSON(path string, out any) error {
	if err := platform.CheckNoSymlinks(path); err != nil {
		return ErrConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, (16<<20)+1))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(out); err != nil {
		return ErrConfig
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return ErrConfig
	}
	return nil
}
func (m *Manager) readDiscovery(ctx context.Context, id model.ProfileID) (discoverySettings, discovery.Snapshot, error) {
	var settings discoverySettings
	var snapshot discovery.Snapshot
	root, err := m.discoveryDir(ctx, id)
	if err != nil {
		return settings, snapshot, err
	}
	if err = readDiscoveryJSON(filepath.Join(root, "settings.json"), &settings); os.IsNotExist(err) {
		return settings, snapshot, nil
	} else if err != nil {
		return settings, snapshot, err
	}
	if err = readDiscoveryJSON(filepath.Join(root, "snapshot.json"), &snapshot); os.IsNotExist(err) {
		snapshot = discovery.Snapshot{Binding: settings.Binding}
		err = nil
	}
	if err == nil && snapshot.Binding != settings.Binding {
		return settings, snapshot, model.ErrIdentityChanged
	}
	return settings, snapshot, err
}
func (m *Manager) writeDiscovery(ctx context.Context, id model.ProfileID, settings discoverySettings, snapshot discovery.Snapshot) error {
	if settings.Binding.ProfileID != string(id) || settings.Binding != snapshot.Binding {
		return model.ErrIdentityChanged
	}
	root, err := m.discoveryDir(ctx, id)
	if err != nil {
		return err
	}
	// Snapshot first: failure never destroys the previously accepted catalog.
	if err = atomicJSON(filepath.Join(root, "snapshot.json"), snapshot); err != nil {
		return ErrConfig
	}
	if err = atomicJSON(filepath.Join(root, "settings.json"), settings); err != nil {
		return ErrConfig
	}
	return nil
}
func (m *Manager) lockDiscovery(ctx context.Context, id model.ProfileID) (func(), error) {
	root, err := m.discoveryDir(ctx, id)
	if err != nil {
		return nil, err
	}
	lock, err := privateLock(filepath.Join(root, "operation.lock"))
	if err != nil {
		return nil, ErrConfig
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ok, err := lock.TryLockContext(waitCtx, 10*time.Millisecond)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrPending
		}
		return nil, err
	}
	if !ok {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrPending
	}
	return func() { _ = lock.Unlock() }, nil
}
func discoveryStateError(err error) string {
	var cliErr *larkcli.Error
	if errors.As(err, &cliErr) && cliErr.Type == "authorization" {
		return "needs_login"
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, discovery.ErrNeedsLogin), errors.Is(err, auth.ErrUserMissing), errors.Is(err, auth.ErrNeedsReconcile), errors.Is(err, auth.ErrMissingScopes):
		return "needs_login"
	case errors.Is(err, discovery.ErrBrowserDisconnected):
		return "browser_disconnected"
	case errors.Is(err, discovery.ErrIdentity), errors.Is(err, model.ErrIdentityChanged), errors.Is(err, larkcli.ErrIdentity):
		return "stale"
	case errors.Is(err, ownedlogin.ErrRuntimeUnavailable):
		return "needs_runtime"
	case errors.Is(err, discovery.ErrProtocol), errors.Is(err, ownedlogin.ErrUnavailable):
		return "unsupported"
	default:
		return "partial"
	}
}
func scanDiscoveryState(ctx context.Context, settings *discoverySettings, session discoverySession, previous discovery.Snapshot) discovery.Snapshot {
	next, err := discovery.ScanWithOptions(ctx, settings.Binding, session, previous, discovery.ScanOptions{ExcludeHiddenShared: !settings.hiddenSharedEnabled()})
	settings.Attempted = next.ScannedAt
	settings.Error = discoveryStateError(err)
	settings.State = "complete"
	if err != nil {
		settings.State = "partial"
		// A personal request requiring login also blocks shared scans with
		// ErrIdentity. That gate must not overwrite the actionable login state.
		if errors.Is(err, discovery.ErrNeedsLogin) {
			settings.State = "needs_login"
		} else if errors.Is(err, discovery.ErrBrowserDisconnected) {
			settings.State = "browser_disconnected"
		} else if errors.Is(err, discovery.ErrIdentity) {
			settings.State = "stale"
		}
	}
	for key, source := range next.Sources {
		if source.Error != "" {
			source.Error = settings.Error
			next.Sources[key] = source
		}
	}
	return next
}
func (m *Manager) discoveryView(ctx context.Context, profile model.Profile) (DiscoveryView, error) {
	out := DiscoveryView{SyncHiddenShared: true, ProfileID: profile.ID, State: "unconfigured", Browser: m.browserBridgeView(profile.ID), Snapshot: discovery.Snapshot{Entries: []discovery.Entry{}, Sources: map[string]discovery.SourceState{}}}
	settings, snapshot, err := m.readDiscovery(ctx, profile.ID)
	if err != nil {
		return out, err
	}
	if settings.Binding.ProfileID != "" {
		out.Configured = true
		out.Enabled = settings.Enabled
		out.SyncHiddenShared = settings.hiddenSharedEnabled()
		out.Origin = settings.Binding.Origin
		out.State = settings.State
		out.Error = settings.Error
		out.LastScan = settings.Attempted
		out.Snapshot = snapshot
		if !out.SyncHiddenShared {
			if out.Snapshot.Sources == nil {
				out.Snapshot.Sources = map[string]discovery.SourceState{}
			}
			state := out.Snapshot.Sources[discovery.SharedHidden]
			state.Status, state.Error = "disabled", ""
			out.Snapshot.Sources[discovery.SharedHidden] = state
		}
		if settings.Binding.ProfileID != string(profile.ID) || settings.Binding.IdentityHash != profile.IdentityHash {
			out.State = "stale"
			out.Error = "stale"
			out.Enabled = false
			out.Snapshot = discovery.Snapshot{Entries: []discovery.Entry{}, Sources: map[string]discovery.SourceState{}}
		}
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return out, err
	}
	for _, remote := range c.Remotes {
		if remote.ProfileID == profile.ID && remote.AccountRoot {
			for _, mount := range c.Mounts {
				if mount.RemoteID == remote.ID {
					out.MountID = mount.ID
					out.Mountpoint = mount.Mountpoint
				}
			}
		}
	}
	return out, nil
}
func (m *Manager) DiscoveryViews(ctx context.Context) ([]DiscoveryView, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DiscoveryView, 0, len(c.Profiles))
	for _, profile := range c.Profiles {
		view, e := m.discoveryView(ctx, profile)
		if e != nil {
			view = DiscoveryView{SyncHiddenShared: true, ProfileID: profile.ID, State: "stale", Error: "stale"}
		}
		out = append(out, view)
	}
	return out, nil
}
func accountToken(id model.ProfileID) string { return "drivebridge-account-" + string(id) }

// RecreateAccountMount is an explicit user action; background discovery never
// clears a removal intent or silently recreates a deleted account drive.
func (m *Manager) RecreateAccountMount(ctx context.Context, id model.ProfileID) error {
	if err := m.admittedConfigUpdate(ctx, func(c *config.Config) error {
		for i := range c.Remotes {
			if c.Remotes[i].ProfileID == id && c.Remotes[i].AccountRoot {
				c.Remotes[i].MountRemoved = false
				return nil
			}
		}
		return ErrConfig
	}); err != nil {
		return err
	}
	p, err := m.ResolveProfile(ctx, string(id))
	if err != nil {
		return err
	}
	mount, err := m.ensureAccountMount(ctx, p)
	if err != nil {
		return err
	}
	_, err = m.Start(ctx, mount.ID, true)
	return err
}
func (m *Manager) ensureAccountMount(ctx context.Context, profile model.Profile) (model.Mount, error) {
	var mount model.Mount
	err := m.admittedConfigUpdate(ctx, func(c *config.Config) error {
		var remote model.Remote
		for _, r := range c.Remotes {
			if r.ProfileID == profile.ID && r.AccountRoot {
				if remote.ID != "" {
					return ErrConfig
				}
				remote = r
			}
		}
		if remote.ID == "" {
			remote = model.Remote{ID: model.RemoteID(uuid.NewString()), Name: "账号盘-" + string(profile.ID), ProfileID: profile.ID, FolderToken: accountToken(profile.ID), NewMDPolicy: "docx", AccountRoot: true}
			c.Remotes = append(c.Remotes, remote)
		}
		if remote.FolderToken != accountToken(profile.ID) {
			return ErrConfig
		}
		if remote.MountRemoved {
			return ErrArchived
		}
		for _, existing := range c.Mounts {
			if existing.RemoteID == remote.ID {
				if mount.ID != "" {
					return ErrConfig
				}
				mount = existing
			}
		}
		if mount.ID != "" {
			if mount.Archived {
				return ErrArchived
			}
			return nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return ErrConfig
		}
		mount = config.DefaultMount()
		mount.ID = model.MountID(uuid.NewString())
		mount.Name = remote.Name
		mount.RemoteID = remote.ID
		mount.Mountpoint = filepath.Join(home, "DriveBridge", string(profile.ID))
		mount.CacheMode = "full"
		mount.Autostart = true
		// Prepare the automatic target for the native mount implementation; WinFsp
		// requires the leaf to remain absent, while Unix mounts need an empty folder.
		if err = prepareAccountMountpoint(mount.Mountpoint); err != nil {
			return ErrConfig
		}
		c.Mounts = append(c.Mounts, mount)
		return nil
	})
	return mount, err
}
func (m *Manager) accountOptions(ctx context.Context, spec mountSpec) (*overlay.AccountOptions, error) {
	if !spec.Remote.AccountRoot {
		return nil, nil
	}
	settings, snapshot, err := m.readDiscovery(ctx, spec.Profile.ID)
	if err != nil {
		return nil, err
	}
	if settings.Binding.ProfileID != string(spec.Profile.ID) || settings.Binding.IdentityHash != spec.Profile.IdentityHash || settings.RootToken == "" || spec.Remote.FolderToken != accountToken(spec.Profile.ID) || snapshot.Binding != settings.Binding {
		return nil, model.ErrIdentityChanged
	}
	binding := settings.Binding
	rootToken := settings.RootToken
	return &overlay.AccountOptions{AccountToken: spec.Remote.FolderToken, PersonalToken: settings.RootToken, SyncHiddenShared: func(ctx context.Context) (bool, error) {
		profile, err := m.ResolveProfile(ctx, string(spec.Profile.ID))
		if err != nil {
			return false, err
		}
		settings, snapshot, err := m.readDiscovery(ctx, spec.Profile.ID)
		if err != nil {
			return false, err
		}
		if profile.IdentityHash != binding.IdentityHash || settings.Binding != binding || snapshot.Binding != binding || settings.RootToken != rootToken {
			return false, model.ErrIdentityChanged
		}
		return settings.hiddenSharedEnabled(), nil
	}, ReadSnapshot: func(ctx context.Context) (discovery.Snapshot, error) {
		settings, snapshot, err := m.readDiscovery(ctx, spec.Profile.ID)
		if err != nil {
			return snapshot, err
		}
		if settings.Binding != binding || snapshot.Binding != binding || settings.RootToken != rootToken {
			return discovery.Snapshot{}, model.ErrIdentityChanged
		}
		return snapshot, nil
	}}, nil
}

func (m *Manager) failedDiscoveryCompletion(ctx context.Context, profile model.Profile, settings discoverySettings, snapshot discovery.Snapshot, cause error) (DiscoveryView, error) {
	settings.State = "partial"
	settings.Error = discoveryStateError(cause)
	if settings.Error == "needs_login" || settings.Error == "stale" {
		settings.State = settings.Error
	}
	if errors.Is(cause, ErrDriver) {
		settings.State = "unsupported"
		settings.Error = "unsupported"
	}
	if err := m.writeDiscovery(ctx, profile.ID, settings, snapshot); err != nil {
		return DiscoveryView{}, err
	}
	view, err := m.discoveryView(ctx, profile)
	if err != nil {
		return view, err
	}
	return view, cause
}
