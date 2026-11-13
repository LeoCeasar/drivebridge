package manager

import (
	"context"
	"errors"
	"io"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/config"
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

// Finder uses the volume label, not the mount directory, as its display name.
// A local macFUSE kernel volume participates in Finder's local-volume handling;
// sidebar visibility still depends on Finder preferences. FSKit does not support
// all kernel mount flags, so keep its existing backend options unchanged.
func nativeFinderOptions(spec mountSpec, goos, engine string, base mountlib.Options) mountlib.Options {
	if goos != "darwin" || engine != "cmount" {
		return base
	}
	if base.VolumeName == "" {
		label := spec.Mount.Name
		if spec.Remote.AccountRoot {
			label = spec.Profile.Name
		}
		// Commas delimit FUSE -o options. Never allow a display name to become
		// another mount option; remove path separators and control bytes too.
		label = strings.Map(func(r rune) rune {
			if strings.ContainsRune(",:/\\", r) || unicode.IsControl(r) {
				return ' '
			}
			return r
		}, label)
		label = strings.Join(strings.Fields(label), " ")
		base.VolumeName = "DriveBridge"
		if label != "" {
			base.VolumeName += " — " + label
		}
		for len(base.VolumeName) > 255 {
			_, size := utf8.DecodeLastRuneInString(base.VolumeName)
			base.VolumeName = base.VolumeName[:len(base.VolumeName)-size]
		}
	}
	local, fskit := false, spec.Mount.MacOSBackend == "fskit"
	for _, option := range base.ExtraOptions {
		for _, part := range strings.Split(option, ",") {
			local = local || part == "local"
			fskit = fskit || part == "backend=fskit"
		}
	}
	if !local && !fskit {
		base.ExtraOptions = append(append([]string(nil), base.ExtraOptions...), "local")
	}
	return base
}

type nativeRuntime struct {
	mu              sync.Mutex
	closed          bool
	v               *vfs.VFS
	point           *mountlib.MountPoint
	closeOnce       sync.Once
	closeErr        error
	unmountOnce     sync.Once
	unmountErr      error
	adapterShutdown bool
}

func (n *nativeRuntime) Activity() (int, int, int)              { return n.v.ManagedActivity() }
func (n *nativeRuntime) StopActivity() (int, int, int)          { return n.v.ManagedStopActivity() }
func (n *nativeRuntime) CachePending() bool                     { return n.v.ManagedCachePending() }
func (n *nativeRuntime) LocalSaveStatus() (bool, int, []string) { return n.v.ManagedLocalSaveStatus() }
func (n *nativeRuntime) LocalSaveFailures() []string            { return n.v.ManagedLocalSaveFailures() }
func (n *nativeRuntime) Pause() (func(), error)                 { return n.v.PauseManagedAdmission() }
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
		// The filesystem's media worker owns the Store too. Join it while
		// RunWorker still owns the open Store, including adapter-led unmounts.
		if features := n.v.Fs().Features(); features != nil && features.Shutdown != nil {
			n.closeErr = features.Shutdown(context.Background())
		}
	})
	return n.closeErr
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
	mountOptions = nativeFinderOptions(spec, runtime.GOOS, engineName, mountOptions)
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
	account, err := m.accountOptions(ctx, spec)
	if err != nil {
		return nil, err
	}
	f, err := overlay.New(overlay.Dependencies{Store: s, Cloud: client, ProfileID: spec.Profile.ID, MountID: spec.Mount.ID, Account: account,
		DeletionCommit: func(context.Context, model.Entry, func() (bool, error)) error { return store.ErrBusy },
	})
	if err != nil {
		return nil, err
	}
	overlayFS := f.(*overlay.Fs)
	defer func() {
		if !success {
			_ = overlayFS.Shutdown(context.Background())
		}
	}()
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
	cacheAge, err := config.EffectiveCacheMaxAge(spec.Mount)
	if err != nil {
		return nil, ErrConfig
	}
	vfsAge := cacheAge
	if vfsAge == 0 {
		vfsAge = time.Duration(1<<63 - 1)
	}
	options.CacheMaxAge = fs.Duration(vfsAge)
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
	deletionCommit := func(ctx context.Context, entry model.Entry, commit func() (bool, error)) error {
		err := v.TryExternalReconcile(ctx, entry.Remote, func() (fs.Object, error) {
			removed, err := commit()
			if err != nil || removed {
				return nil, err
			}
			return overlayFS.NewObject(ctx, entry.Remote)
		})
		if err == nil {
			parent := path.Dir(entry.Remote)
			if parent == "." {
				parent = ""
			}
			v.NotifyManagedDirectoryChanged(parent)
		}
		return err
	}
	overlayFS.SetDeletionCommit(deletionCommit)
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
	engine, err := syncer.New(syncer.Dependencies{DeletionCommit: deletionCommit, CheckIdentity: func(ctx context.Context) error {
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
	}), ShouldRefresh: func(ctx context.Context, id model.EntryID) (bool, error) {
		if allowed, err := overlayFS.SyncEntryAllowed(ctx, id); err != nil || !allowed {
			return allowed, err
		}
		return backgroundPullAllowed(ctx, m.LoadConfig, s.Entry, spec.Mount.ID, id)
	}, ShouldSync: overlayFS.ShouldSync, ShouldSyncPath: overlayFS.SyncPathAllowed, LocalOnly: overlayFS.LocalOnly, ActiveEntries: overlayFS.ActiveEntries}, syncer.Options{PollInterval: poll, DirectoryInterval: dirs})
	if err != nil {
		native.Unmount()
		return nil, err
	}
	indexWake := make(chan struct{}, 1)
	indexedRun := metadataIndexedRun(engine.Run, func(ctx context.Context) error {
		return overlayFS.WarmDirectoriesWithNotify(ctx, v.NotifyManagedDirectoryChanged)
	}, dirs, indexWake)
	var cacheObserved atomic.Int64
	cacheObserved.Store(-1)
	backgroundRun := metadataIndexedRun(indexedRun, func(ctx context.Context) error {
		result, err := sweepCleanBodies(ctx, s, v, overlayFS.NewObject, cacheAge, spec.Mount.CacheMaxBytes, time.Now().UTC())
		if err == nil {
			cacheObserved.Store(result.BytesAfter)
		}
		return err
	}, 5*time.Minute)
	success = true
	return &workerServices{restoreHistory: func(ctx context.Context, req model.HistoryRestoreRequest, body io.Reader, bundle *store.HistoryRestoreBundle) (out model.HistoryRestoreResult, err error) {
		allowed, e := overlayFS.SyncPathAllowed(ctx, req.ExpectedPath)
		if e != nil {
			return out, e
		}
		if !allowed {
			return out, model.ErrUnsupportedContent
		}
		err = v.TryExternalUpdate(ctx, req.ExpectedPath, func() (fs.Object, error) {
			var e error
			out, e = s.RestoreHistory(ctx, req, body, bundle)
			if e != nil {
				return nil, e
			}
			return overlayFS.NewObject(context.WithoutCancel(ctx), out.Entry.Remote)
		})
		if errors.Is(err, vfs.ErrExternalUpdateBusy) {
			return out, ErrPending
		}
		return
	}, store: s, runtime: native, run: backgroundRun, syncNow: engine.SyncNow, cacheBytes: func() (int64, bool) { n := cacheObserved.Load(); return n, n >= 0 }, directoryStatus: func(ctx context.Context) (model.DirectoryIndexStatus, error) {
		stats, err := overlayFS.DirectoryStatus(ctx)
		return model.DirectoryIndexStatus{Known: stats.Known, Cached: stats.Cached, Refreshing: stats.Refreshing, Failed: stats.Failed}, err
	}, refreshConflict: engine.RefreshConflict, refreshDirectories: func(ctx context.Context) error {
		if err := overlayFS.RefreshCatalog(ctx); err != nil {
			return err
		}
		select {
		case indexWake <- struct{}{}:
		default:
		}
		root, err := v.Root()
		if err != nil {
			return err
		}
		// VFS retains open and dirty virtual entries while expiring safe rows.
		root.ForgetAll()
		// A dirty descendant can prevent ForgetAll from expiring its parents.
		// Invalidate their timestamps without removing the open/dirty nodes.
		root.ForgetPath(".drivebridge-refresh", fs.EntryObject)
		if spec.Remote.AccountRoot {
			for _, category := range []string{"个人目录", "共享目录", "手动添加"} {
				root.ForgetPath(category+"/.drivebridge-refresh", fs.EntryObject)
			}
		}
		return nil
	}, resumeCredentials: func(ctx context.Context) error { return resumeVerifiedCredentials(ctx, s) }, resolve: func(ctx context.Context, r model.ResolveRequest) error {
		_, _, err := engine.Resolve(ctx, r)
		return err
	}, retry: func(ctx context.Context) error {
		if err := v.RetryManagedLocalSaves(ctx); err != nil {
			return err
		}
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
