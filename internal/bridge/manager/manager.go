// Package manager owns per-user mount workers and authenticated local control.
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

var (
	ErrConfig          = errors.New("invalid product configuration")
	ErrDriver          = errors.New("mount driver or compiled engine unavailable")
	ErrPending         = errors.New("active handles or unconfirmed data prevent operation")
	ErrArchived        = errors.New("mount configuration is archived; restore it before starting")
	ErrInstanceChanged = errors.New("worker identity changed")
	ErrUnavailable     = errors.New("worker unavailable")
	ErrReadyTimeout    = errors.New("worker has not become ready")
)

type Options struct {
	ConfigFile, CLIExecutable, Executable string
	GitExecutable                         string
	ReadyTimeout                          time.Duration
}
type mountSpec struct {
	Mount   model.Mount
	Remote  model.Remote
	Profile model.Profile
	Paths   config.MountPaths
	Config  config.Config
}
type workerFactory func(context.Context, mountSpec) (*workerServices, error)
type dependencies struct {
	domain         string
	factory        workerFactory
	startLease     func(context.Context) (func(), error)
	lifecycleLease func(context.Context) (func(), error)
}
type Manager struct {
	historyRepositories map[model.ProfileID]historyRepositoryCache
	options             Options
	domain              string
	factory             workerFactory
	spawn               func(context.Context, []string, string) (int, error)
	startLease          func(context.Context) (func(), error)
	lifecycleLease      func(context.Context) (func(), error)
	mu                  sync.Mutex
	workers             map[model.MountID]*worker
	auth                *auth.Service
	ctx                 context.Context
	cancel              context.CancelFunc
	discoveryMu         sync.Mutex
	discoverySessions   map[model.ProfileID]discoverySession
	discoveryFactory    discoverySessionFactory
	discoveryClosing    bool
	discoveryLogins     map[model.ProfileID]context.CancelFunc
	discoveryWG         sync.WaitGroup
	automaticMu         sync.Mutex
	automaticClosing    bool
	automaticWG         sync.WaitGroup
	automaticProfiles   map[model.ProfileID]*automaticLogin
	automaticFlows      map[string]*automaticLogin
	automaticDeps       *automaticLoginDependencies
	closeOnce           sync.Once
	closeErr            error
}

func New(o Options) (*Manager, error) { return newManager(o, dependencies{}) }
func newManager(o Options, d dependencies) (*Manager, error) {
	p, err := config.DefaultPaths()
	if err != nil {
		return nil, err
	}
	if o.ConfigFile == "" {
		o.ConfigFile = p.ConfigFile
	}
	o.ConfigFile, err = filepath.Abs(o.ConfigFile)
	if err != nil {
		return nil, err
	}
	rules, err := markdown.LoadConversionRules(markdown.ConversionRulesPath(o.ConfigFile))
	if err != nil {
		return nil, err
	}
	if err = markdown.ConfigureConversionRules(rules); err != nil {
		return nil, err
	}
	if o.Executable == "" {
		o.Executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	o.Executable, err = filepath.Abs(o.Executable)
	if err != nil {
		return nil, err
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 30 * time.Second
	}
	if d.domain == "" {
		d.domain, err = auth.DefaultDomain()
		if err != nil {
			return nil, err
		}
	}
	if d.startLease == nil {
		d.startLease = func(ctx context.Context) (func(), error) {
			c, err := auth.NewCoordinator()
			if err != nil {
				return nil, err
			}
			return c.AcquireWorkerStart(ctx)
		}
	}

	if d.lifecycleLease == nil {
		d.lifecycleLease = func(ctx context.Context) (func(), error) {
			c, err := auth.NewCoordinator()
			if err != nil {
				return nil, err
			}
			return c.AcquireLifecycle(ctx)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{options: o, domain: d.domain, factory: d.factory, startLease: d.startLease, lifecycleLease: d.lifecycleLease, workers: map[model.MountID]*worker{}, ctx: ctx, cancel: cancel}
	if m.factory == nil {
		m.factory = m.buildWorker
	}
	m.spawn = func(ctx context.Context, args []string, log string) (int, error) {
		return platform.SpawnDetached(ctx, m.options.Executable, args, log)
	}
	return m, nil
}
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.closeAutomaticLogins()
		// Native tray quit may terminate the process immediately after this returns.
		// Close discovery transports before canceling their lifetime, and make repeated
		// callers wait for the same shutdown. Owned login helpers are drained before native tray termination.
		m.closeDiscoverySessions()
		m.cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.auth != nil {
			m.closeErr = m.auth.Close()
		}
	})
	return m.closeErr
}
func (m *Manager) ConfigFile() string { return m.options.ConfigFile }
func (m *Manager) LoadConfig(ctx context.Context) (config.Config, error) {
	if err := ctx.Err(); err != nil {
		return config.Config{}, err
	}
	return config.Load(m.options.ConfigFile)
}
func (m *Manager) paths(c config.Config) (config.Paths, error) {
	p, err := config.DefaultPaths()
	if err != nil {
		return p, err
	}
	p.ConfigFile = m.options.ConfigFile
	if c.StateDir != "" {
		p.StateDir = c.StateDir
	}
	if c.CacheDir != "" {
		p.CacheDir = c.CacheDir
	}
	return p, nil
}
func (m *Manager) spec(ctx context.Context, id model.MountID) (mountSpec, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return mountSpec{}, err
	}
	s := mountSpec{Config: c}
	found := false
	for _, v := range c.Mounts {
		if v.ID == id {
			s.Mount = v
			found = true
		}
	}
	if !found {
		return s, ErrConfig
	}
	for _, v := range c.Remotes {
		if v.ID == s.Mount.RemoteID {
			s.Remote = v
		}
	}
	for _, v := range c.Profiles {
		if v.ID == s.Remote.ProfileID {
			s.Profile = v
		}
	}
	if s.Profile.ID == "" {
		return s, ErrConfig
	}
	p, err := m.paths(c)
	if err != nil {
		return s, err
	}
	s.Paths, err = p.MountPaths(id)
	return s, err
}
func (m *Manager) ResolveMount(ctx context.Context, name string) (model.Mount, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return model.Mount{}, err
	}
	for _, v := range c.Mounts {
		if v.Name == name || string(v.ID) == name {
			return v, nil
		}
	}
	return model.Mount{}, ErrConfig
}
func (m *Manager) ResolveRemote(ctx context.Context, name string) (model.Remote, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return model.Remote{}, err
	}
	for _, v := range c.Remotes {
		if v.Name == name || string(v.ID) == name {
			return v, nil
		}
	}
	return model.Remote{}, ErrConfig
}
func (m *Manager) ResolveProfile(ctx context.Context, name string) (model.Profile, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return model.Profile{}, err
	}
	for _, v := range c.Profiles {
		if v.Name == name || string(v.ID) == name {
			return v, nil
		}
	}
	return model.Profile{}, ErrConfig
}
func privateLock(path string) (*flock.Flock, error) {
	if err := platform.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := platform.CheckNoSymlinks(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = platform.SecurePrivate(path, false); err != nil {
		return nil, err
	}
	return flock.New(path), nil
}
func (m *Manager) updateConfig(ctx context.Context, update func(*config.Config) error) error {
	l, err := privateLock(m.options.ConfigFile + ".manager.lock")
	if err != nil {
		return err
	}
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPending
	}
	defer l.Unlock()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return err
	}
	if err = update(&c); err != nil {
		return err
	}
	paths, pathErr := m.paths(c)
	if pathErr != nil {
		return pathErr
	}
	if validationErr := config.Validate(c, paths); validationErr != nil {
		return errors.Join(ErrConfig, validationErr)
	}
	return config.Save(m.options.ConfigFile, c)
}

// InitializeConfig persists the loaded configuration and claims its private roots
// before a desktop host creates runtime files there. It follows the same lifecycle,
// manager/config, and private-root lock order as other public config writers.
func (m *Manager) InitializeConfig(ctx context.Context) error {
	if err := m.admittedConfigUpdate(ctx, func(*config.Config) error { return nil }); err != nil {
		return err
	}
	return markdown.EnsureConversionRulesFile(m.options.ConfigFile)
}

func (m *Manager) AddProfile(ctx context.Context, name, brand string) (model.Profile, error) {
	p := model.Profile{ID: model.ProfileID(uuid.NewString()), Name: name, Brand: brand}
	err := m.admittedConfigUpdate(ctx, func(c *config.Config) error { c.Profiles = append(c.Profiles, p); return nil })
	return p, err
}
func (m *Manager) AddRemote(ctx context.Context, name string, profile model.ProfileID, folder string) (model.Remote, error) {
	r := model.Remote{ID: model.RemoteID(uuid.NewString()), Name: name, ProfileID: profile, FolderToken: folder, NewMDPolicy: "docx"}
	err := m.admittedConfigUpdate(ctx, func(c *config.Config) error { c.Remotes = append(c.Remotes, r); return nil })
	return r, err
}
func (m *Manager) AddMount(ctx context.Context, v model.Mount) (model.Mount, error) {
	if v.ID == "" {
		v.ID = model.MountID(uuid.NewString())
	}
	err := m.admittedConfigUpdate(ctx, func(c *config.Config) error { c.Mounts = append(c.Mounts, v); return nil })
	return v, err
}
func (m *Manager) RemoveRemote(ctx context.Context, id model.RemoteID) error {
	return m.admittedConfigUpdate(ctx, func(c *config.Config) error {
		for _, v := range c.Mounts {
			if v.RemoteID == id {
				return ErrPending
			}
		}
		out := c.Remotes[:0]
		found := false
		for _, v := range c.Remotes {
			if v.ID == id {
				found = true
			} else {
				out = append(out, v)
			}
		}
		if !found {
			return ErrConfig
		}
		c.Remotes = out
		return nil
	})
}

type profileStore struct{ m *Manager }

func (p profileStore) Profile(ctx context.Context, id model.ProfileID) (model.Profile, error) {
	return p.m.ResolveProfile(ctx, string(id))
}
func (p profileStore) Bind(ctx context.Context, id model.ProfileID, ref, hash string) error {
	return p.m.updateConfig(ctx, func(c *config.Config) error {
		for i := range c.Profiles {
			if c.Profiles[i].ID == id {
				c.Profiles[i].AuthRef = ref
				c.Profiles[i].IdentityHash = hash
				return nil
			}
		}
		return ErrConfig
	})
}

var RequiredScopes = []string{"drive:drive.metadata:readonly", "drive:file:download", "drive:file:upload", "space:folder:create", "space:document:move", "space:document:delete", "docx:document:readonly", "docx:document:create", "docx:document:write_only", "search:docs:read"}

func (m *Manager) Auth(ctx context.Context) (*auth.Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.auth != nil {
		return m.auth, nil
	}
	exe, err := ResolveCLI(m.options.CLIExecutable, m.options.Executable)
	if err != nil {
		return nil, err
	}
	m.auth, err = auth.New(m.ctx, auth.Options{Executable: exe, ConfigFile: m.options.ConfigFile, Profiles: profileStore{m}, RequiredScopes: append([]string(nil), RequiredScopes...), Guard: m})
	return m.auth, err
}
func atomicJSON(path string, v any) error {
	if err := platform.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := platform.CheckNoSymlinks(path); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = platform.SecurePrivate(name, false); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = platform.ReplaceFile(name, path); err != nil {
		return err
	}
	return platform.SyncDir(filepath.Dir(path))
}
func validID(s string) bool {
	v, e := uuid.Parse(s)
	return e == nil && v != uuid.Nil && v.String() == s
}

// Public config writers share lifecycle admission; auth.ProfileStore.Bind is
// deliberately the raw privileged writer under its already-exclusive lease.
func (m *Manager) admittedConfigUpdate(ctx context.Context, update func(*config.Config) error) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	return m.updateConfig(ctx, update)
}
