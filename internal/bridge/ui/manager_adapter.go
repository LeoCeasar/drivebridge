package ui

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

// ManagerBackend is the sole production UI adapter. It never opens worker databases.
type ManagerBackend struct {
	manager   *manager.Manager
	snapshots statusSnapshots
}

func NewManagerBackend(m *manager.Manager) (*ManagerBackend, error) {
	if m == nil {
		return nil, errors.New("missing manager")
	}
	return &ManagerBackend{manager: m}, nil
}
func (b *ManagerBackend) Config(ctx context.Context) (ConfigView, error) {
	c, err := b.manager.LoadConfig(ctx)
	if err != nil {
		return ConfigView{}, err
	}
	_, canRecover := any(b.manager).(auth.RecoveryGuard)
	v := ConfigView{Platform: runtime.GOOS, CanRecover: canRecover, Profiles: []ProfileView{}, Remotes: []RemoteView{}, Mounts: []MountView{}}
	for _, p := range c.Profiles {
		v.Profiles = append(v.Profiles, ProfileView{ID: p.ID, Name: p.Name, Brand: p.Brand, Authorized: p.AuthRef != "" && p.IdentityHash != ""})
	}
	for _, r := range c.Remotes {
		v.Remotes = append(v.Remotes, RemoteView{ID: r.ID, Name: r.Name, ProfileID: r.ProfileID, FolderToken: r.FolderToken, AccountRoot: r.AccountRoot, MountRemoved: r.MountRemoved})
	}
	for _, m := range c.Mounts {
		age := m.CacheMaxAge
		if age == "" {
			age = config.DefaultCacheMaxAge
		}
		rules, err := config.EffectiveBackgroundPullExcludedExtensions(m)
		if err != nil {
			return ConfigView{}, err
		}
		v.Mounts = append(v.Mounts, MountView{Archived: m.Archived, BackgroundPullExcludedExtensions: rules, ID: m.ID, Name: m.Name, RemoteID: m.RemoteID, Mountpoint: m.Mountpoint, CacheMaxBytes: m.CacheMaxBytes, CacheMaxAge: age})
	}
	return v, nil
}
func (b *ManagerBackend) Status(ctx context.Context) (StatusView, error) {
	c, err := b.Config(ctx)
	if err != nil {
		return StatusView{}, err
	}
	return b.snapshots.read(ctx, c.Mounts, b.manager), nil
}

func (b *ManagerBackend) Configure(ctx context.Context, r ConfigRequest) (ConfigView, error) {
	var err error
	switch r.Action {
	case "profile":
		_, err = b.manager.AddProfile(ctx, r.Name, r.Brand)
	case "remote":
		_, err = b.manager.AddRemote(ctx, r.Name, r.ProfileID, r.FolderToken)
	case "mount_policy":
		if r.BackgroundPullExcludedExtensions == nil {
			return ConfigView{}, manager.ErrConfig
		}
		err = b.manager.SetBackgroundPullPolicy(ctx, r.MountID, *r.BackgroundPullExcludedExtensions)
	case "mount":
		var mount model.Mount
		mount, err = mountFromConfigRequest(r)
		if err == nil {
			_, err = b.manager.AddMount(ctx, mount)
		}
	case "account_mount":
		err = b.manager.RecreateAccountMount(ctx, r.ProfileID)
	default:
		return ConfigView{}, errors.New("unknown configuration action")
	}
	if err != nil {
		return ConfigView{}, err
	}
	return b.Config(ctx)
}
func (b *ManagerBackend) AuthStart(ctx context.Context, id model.ProfileID) (auth.AuthFlow, error) {
	return b.manager.StartAutomaticLogin(ctx, id, false)
}
func (b *ManagerBackend) AuthStartRecovery(ctx context.Context, id model.ProfileID) (auth.AuthFlow, error) {
	return b.manager.StartAutomaticLogin(ctx, id, true)
}
func (b *ManagerBackend) AuthProgress(ctx context.Context, id string) (manager.AutomaticLoginView, error) {
	return b.manager.AutomaticLoginStatus(ctx, id)
}
func (b *ManagerBackend) AuthComplete(ctx context.Context, id string) error {
	service, err := b.manager.Auth(ctx)
	if err != nil {
		return err
	}
	_, err = service.Complete(ctx, id)
	return err
}
func (b *ManagerBackend) Conflict(ctx context.Context, id model.ConflictID) (ConflictInputs, error) {
	v, err := b.manager.Conflict(ctx, id)
	return ConflictInputs{Conflict: v.Conflict, Entry: v.Entry, Base: v.Base, Local: v.Local, Remote: v.Remote, Draft: v.Draft, Text: v.Text, SourceURL: v.SourceURL, Stale: v.Stale, RefreshWarning: v.RefreshWarning}, err
}
func (b *ManagerBackend) Retry(ctx context.Context, id model.MountID) error {
	return b.manager.Retry(ctx, id)
}
func (b *ManagerBackend) ExportConflict(ctx context.Context, id model.ConflictID) (ConflictCopies, error) {
	v, err := b.manager.ExportConflict(ctx, id)
	return ConflictCopies{Directory: v.Directory, Base: v.Base, Local: v.Local, Remote: v.Remote, Draft: v.Draft, Manifest: v.Manifest}, err
}
func (b *ManagerBackend) SaveDraft(ctx context.Context, id model.ConflictID, v []byte) error {
	return b.manager.SaveDraft(ctx, id, v)
}
func (b *ManagerBackend) Resolve(ctx context.Context, r model.ResolveRequest) error {
	return b.manager.ResolveConflict(ctx, r)
}
func (b *ManagerBackend) Mount(ctx context.Context, id model.MountID, action string) error {
	switch action {
	case "delete":
		return b.manager.DeleteMount(ctx, id)
	case "restore_start":
		if err := b.manager.RestoreMount(ctx, id); err != nil {
			return err
		}
		_, err := b.manager.Start(ctx, id, true)
		return err
	case "archive":
		return b.manager.ArchiveMount(ctx, id)
	case "restore":
		return b.manager.RestoreMount(ctx, id)
	case "start":
		_, err := b.manager.Start(ctx, id, true)
		return err
	case "stop":
		return b.manager.Stop(ctx, id, 60*time.Second)
	default:
		return errors.New("unknown mount action")
	}
}
func (b *ManagerBackend) ClaimConflictNotification(ctx context.Context, id model.ConflictID) (bool, error) {
	return b.manager.ClaimConflictNotification(ctx, id)
}

var _ Backend = (*ManagerBackend)(nil)
var _ SyncRetrier = (*ManagerBackend)(nil)
var _ ConflictExporter = (*ManagerBackend)(nil)

func (b *ManagerBackend) UploadLocalCopy(ctx context.Context, mount model.MountID, job model.JobID) (manager.UploadCopy, error) {
	return b.manager.UploadLocalCopy(ctx, mount, job)
}
