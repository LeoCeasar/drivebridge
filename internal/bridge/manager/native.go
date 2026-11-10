package manager

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/overlay"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

func selectMount() (string, mountlib.MountFn, error) {
	name := "cmount"
	if runtime.GOOS == "linux" {
		name = "mount2"
	}
	name, fn := mountlib.ResolveMountMethod(name)
	if fn == nil {
		return name, nil, ErrDriver
	}
	return name, fn, nil
}

type nativeRuntime struct {
	v               *vfs.VFS
	point           *mountlib.MountPoint
	closeOnce       sync.Once
	unmountOnce     sync.Once
	unmountErr      error
	adapterShutdown bool
}

func (n *nativeRuntime) Activity() (int, int, int) { return n.v.ManagedActivity() }
func (n *nativeRuntime) CachePending() bool        { return n.v.ManagedCachePending() }
func (n *nativeRuntime) Pause() (func(), error)    { return n.v.PauseManagedAdmission() }
func (n *nativeRuntime) Unmount() error {
	n.unmountOnce.Do(func() { n.adapterShutdown = true; n.unmountErr = n.point.Unmount() })
	return n.unmountErr
}
func (n *nativeRuntime) Done() <-chan error           { return n.point.ErrChan }
func (n *nativeRuntime) CachePaths() (string, string) { return n.v.ManagedCachePaths() }
func (n *nativeRuntime) Close() error {
	n.closeOnce.Do(func() {
		if !n.adapterShutdown {
			n.v.Shutdown()
		}
	})
	return nil
}
func (m *Manager) buildWorker(ctx context.Context, spec mountSpec) (*workerServices, error) {
	_, mount, err := selectMount()
	if err != nil {
		return nil, err
	}
	if err = checkDriver(); err != nil {
		return nil, err
	}
	a, err := m.Auth(ctx)
	if err != nil {
		return nil, err
	}
	runner, err := a.ManagedRunner(ctx, spec.Profile.ID)
	if err != nil {
		return nil, err
	}
	exe, err := ResolveCLI(m.options.CLIExecutable, m.options.Executable)
	if err != nil {
		return nil, err
	}
	grants, _, err := trustedGrants(exe)
	if err != nil {
		return nil, err
	}
	client, err := cloud.New(runner, cloud.Options{TempDir: filepath.Join(spec.Paths.Root, "transport"), Capabilities: grants})
	if err != nil {
		return nil, err
	}
	s, err := store.Open(spec.Paths.Root)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = s.Close()
		}
	}()
	if err = s.BindScope(ctx, ownershipFor(m.options.ConfigFile, spec).Scope); err != nil {
		return nil, err
	}
	if err = s.Validate(ctx); err != nil {
		return nil, err
	}
	f, err := overlay.New(overlay.Dependencies{Store: s, Cloud: client, ProfileID: spec.Profile.ID, MountID: spec.Mount.ID})
	if err != nil {
		return nil, err
	}
	overlayFS := f.(*overlay.Fs)
	if _, err = f.List(ctx, ""); err != nil {
		return nil, err
	}
	if err = platform.EnsurePrivateDir(spec.Paths.VFS); err != nil {
		return nil, err
	}
	if err = fsconfig.SetCacheDir(spec.Paths.VFS); err != nil {
		return nil, err
	}
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.CacheMaxSize = fs.SizeSuffix(spec.Mount.CacheMaxBytes)
	duration, err := time.ParseDuration(spec.Mount.WriteBack)
	if err != nil {
		return nil, ErrConfig
	}
	options.WriteBack = fs.Duration(duration)
	options.DirCacheTime = fs.Duration(30 * time.Second)
	options.Refresh = false
	v, err := vfs.NewManaged(ctx, f, &options)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !success {
			v.Shutdown()
		}
	}()
	mountOptions := mountlib.Opt
	mountOptions.Daemon = false
	point := mountlib.NewMountPoint(mount, spec.Mount.Mountpoint, f, &mountOptions, &options)
	point.VFS = v
	point.ErrChan, point.UnmountFn, _, err = mount(v, point.MountPoint, &point.MountOpt)
	if err != nil {
		return nil, errors.Join(ErrDriver, err)
	}
	point.MountedOn = time.Now()
	native := &nativeRuntime{v: v, point: point}
	poll, err := time.ParseDuration(spec.Mount.PollDocs)
	if err != nil {
		native.Unmount()
		return nil, ErrConfig
	}
	dirs, err := time.ParseDuration(spec.Mount.RefreshDirs)
	if err != nil {
		native.Unmount()
		return nil, ErrConfig
	}
	engine, err := syncer.New(syncer.Dependencies{Store: s, Cloud: client, Publisher: syncer.PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		entry, err := s.Entry(ctx, c.EntryID)
		if err != nil {
			return err
		}
		return v.TryExternalUpdate(ctx, entry.Remote, func() (fs.Object, error) { return overlayFS.Publish(ctx, c) })
	}), ShouldSync: overlayFS.ShouldSync, LocalOnly: overlayFS.LocalOnly, ActiveEntries: overlayFS.ActiveEntries, RefreshDirectories: overlayFS.RefreshDirectories}, syncer.Options{PollInterval: poll, DirectoryInterval: dirs})
	if err != nil {
		native.Unmount()
		return nil, err
	}
	success = true
	return &workerServices{store: s, runtime: native, run: engine.Run, resolve: func(ctx context.Context, r model.ResolveRequest) error {
		_, _, err := engine.Resolve(ctx, r)
		return err
	}, retry: func(ctx context.Context) error {
		jobs, err := s.Pending(ctx)
		if err != nil {
			return err
		}
		seen := map[model.EntryID]bool{}
		var failures error
		for _, j := range jobs {
			if !seen[j.EntryID] {
				seen[j.EntryID] = true
				failures = errors.Join(failures, engine.Retry(ctx, j.EntryID))
			}
		}
		return failures
	}}, nil
}
