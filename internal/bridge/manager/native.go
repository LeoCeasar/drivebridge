package manager

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/auth"
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

// Backend selection is per mount. Kernel leaves macFUSE's current default in
// place; FSKit is opt-in and only supported by the macOS cmount adapter.
func nativeMountOptions(backend, goos, engine string, base mountlib.Options) (mountlib.Options, error) {
	if backend == "" {
		return base, nil
	}
	if goos != "darwin" || engine != "cmount" || backend != "kernel" && backend != "fskit" {
		return base, ErrConfig
	}
	if backend == "fskit" {
		base.ExtraOptions = append(append([]string(nil), base.ExtraOptions...), "backend=fskit")
		// Feishu stores file contents rather than macOS resource forks. FSKit
		// needs the upstream cmount noapplexattr compatibility path for Finder.
		base.NoAppleXattr = true
	}
	return base, nil
}

type nativeRuntime struct {
	mu              sync.Mutex
	closed          bool
	v               *vfs.VFS
	point           *mountlib.MountPoint
	closeOnce       sync.Once
	unmountOnce     sync.Once
	unmountErr      error
	adapterShutdown bool
}

func (n *nativeRuntime) Activity() (int, int, int)     { return n.v.ManagedActivity() }
func (n *nativeRuntime) StopActivity() (int, int, int) { return n.v.ManagedStopActivity() }
func (n *nativeRuntime) CachePending() bool            { return n.v.ManagedCachePending() }
func (n *nativeRuntime) Pause() (func(), error)        { return n.v.PauseManagedAdmission() }
func (n *nativeRuntime) Unmount() error {
	n.unmountOnce.Do(func() {
		n.mu.Lock()
		if n.closed {
			n.unmountErr = ErrUnavailable
			n.mu.Unlock()
			return
		}
		n.adapterShutdown = true
		n.mu.Unlock()
		n.unmountErr = n.point.Unmount()
	})
	return n.unmountErr
}
func (n *nativeRuntime) Done() <-chan error           { return n.point.ErrChan }
func (n *nativeRuntime) CachePaths() (string, string) { return n.v.ManagedCachePaths() }
func (n *nativeRuntime) Close() error {
	n.closeOnce.Do(func() {
		n.mu.Lock()
		n.closed = true
		adapterShutdown := n.adapterShutdown
		n.mu.Unlock()
		if !adapterShutdown {
			n.v.Shutdown()
		}
	})
	return nil
}
func (m *Manager) buildWorker(ctx context.Context, spec mountSpec) (*workerServices, error) {
	engineName, mount, err := selectMount()
	if err != nil {
		return nil, err
	}
	mountOptions, err := nativeMountOptions(spec.Mount.MacOSBackend, runtime.GOOS, engineName, mountlib.Opt)
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
	verifyIdentity := func(ctx context.Context) error {
		identity, err := a.Status(ctx, spec.Profile.ID)
		if err != nil {
			return err
		}
		if auth.IdentityHash(identity.Brand, identity.AppID, identity.UserOpenID) != spec.Profile.IdentityHash {
			return model.ErrIdentityChanged
		}
		return nil
	}
	transportDir := filepath.Join(spec.Paths.Root, "transport")
	if err = platform.EnsurePrivateDir(transportDir); err != nil {
		return nil, err
	}
	client, err := cloud.New(runner, cloud.Options{CheckMutation: func(context.Context) error {
		if len(grants) == 0 {
			return model.ErrUnsupportedContent
		}
		return nil
	}, CheckIdentity: verifyIdentity, TempDir: transportDir, Capabilities: grants})
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
	var native *nativeRuntime
	defer func() {
		if !success {
			if native != nil {
				_ = native.Unmount()
				_ = native.Close()
			} else {
				v.Shutdown()
			}
		}
	}()
	mountOptions.Daemon = false
	point := mountlib.NewMountPoint(mount, spec.Mount.Mountpoint, f, &mountOptions, &options)
	point.VFS = v
	point.SetVolumeName(point.MountOpt.VolumeName)
	point.SetDeviceName(point.MountOpt.DeviceName)
	point.ErrChan, point.UnmountFn, _, err = mount(v, point.MountPoint, &point.MountOpt)
	if err != nil {
		return nil, errors.Join(ErrDriver, err)
	}
	point.MountedOn = time.Now()
	native = &nativeRuntime{v: v, point: point}
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
	engine, err := syncer.New(syncer.Dependencies{CheckIdentity: func(ctx context.Context) error {
		binding, err := a.Resolve(ctx, spec.Profile.ID)
		if err != nil {
			return err
		}
		if binding.IdentityHash != spec.Profile.IdentityHash {
			return model.ErrIdentityChanged
		}
		return nil
	}, OnConflict: func(ctx context.Context, c model.Conflict) error { return m.notifyConflict(ctx, spec, s, c) }, Store: s, Cloud: client, Publisher: syncer.PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		entry, err := s.Entry(ctx, c.EntryID)
		if err != nil {
			return err
		}
		return v.TryExternalUpdate(ctx, entry.Remote, func() (fs.Object, error) { return overlayFS.Publish(ctx, c) })
	}), ShouldSync: overlayFS.ShouldSync, LocalOnly: overlayFS.LocalOnly, ActiveEntries: overlayFS.ActiveEntries, RefreshDirectories: overlayFS.RefreshDirectories}, syncer.Options{WriteBack: duration, PollInterval: poll, DirectoryInterval: dirs})
	if err != nil {
		native.Unmount()
		return nil, err
	}
	success = true
	return &workerServices{store: s, runtime: native, run: engine.Run, refreshConflict: engine.RefreshConflict, resumeCredentials: func(ctx context.Context) error { return resumeVerifiedCredentials(ctx, s) }, resolve: func(ctx context.Context, r model.ResolveRequest) error {
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

func resumeVerifiedCredentials(ctx context.Context, s *store.Store) error {
	jobs, err := s.Pending(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if job.State != model.StatusBlocked && job.State != model.StatusPaused {
			continue
		}
		journal, err := s.Journal(ctx, job.ID)
		if err != nil {
			return err
		}
		if journal.ErrorCode != "identity_changed" && journal.ErrorCode != "permission_denied" {
			continue
		}
		if err = s.ResumeJob(ctx, job.ID); err != nil && !errors.Is(err, store.ErrInvalidState) {
			return err
		}
	}
	return nil
}
