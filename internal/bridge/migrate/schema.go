// Package migrate provides guarded migration/recovery primitives. A composition
// layer must stop mutations, drain local cache and hold lifecycle exclusion.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

var (
	ErrUnknownSchema  = errors.New("unsupported migration schema: read only")
	ErrNotQuiescent   = errors.New("exclusive lifecycle quiescence required")
	ErrPreviewChanged = errors.New("import preview changed or was not explicitly accepted")
)

// Quiescer must exclude config changes, worker mutation and open/dirty VFS use
// throughout fn. It must fail on stop/flush timeout. No no-op implementation is supplied.
type Quiescer interface {
	WithQuiescence(context.Context, func(context.Context) error) error
}
type Mount struct {
	Store *store.Store
	Dir   string
}
type Options struct {
	Paths  config.Paths
	Guard  Quiescer
	Mounts []Mount
}
type Service struct {
	paths  config.Paths
	guard  Quiescer
	mounts []Mount
}

func New(o Options) (*Service, error) {
	if o.Guard == nil {
		return nil, ErrNotQuiescent
	}
	for _, p := range []string{o.Paths.ConfigFile, o.Paths.StateDir, o.Paths.CacheDir} {
		if !filepath.IsAbs(p) {
			return nil, errors.New("absolute lifecycle paths required")
		}
		if err := platform.CheckNoSymlinks(p); err != nil {
			return nil, err
		}
	}
	for _, m := range o.Mounts {
		if m.Store == nil || !filepath.IsAbs(m.Dir) {
			return nil, errors.New("invalid mount source")
		}
		if err := platform.CheckNoSymlinks(m.Dir); err != nil {
			return nil, err
		}
	}
	return &Service{paths: o.Paths, guard: o.Guard, mounts: append([]Mount(nil), o.Mounts...)}, nil
}
func binding(c model.Config, sc store.Scope) error {
	var profile model.Profile
	var remote model.Remote
	var mount model.Mount
	for _, v := range c.Profiles {
		if v.ID == sc.ProfileID {
			profile = v
		}
	}
	for _, v := range c.Remotes {
		if v.ID == sc.RemoteID {
			remote = v
		}
	}
	for _, v := range c.Mounts {
		if v.ID == sc.MountID {
			mount = v
		}
	}
	if profile.ID == "" || profile.IdentityHash != sc.IdentityHash || profile.AuthRef == "" || remote.ID == "" || remote.ProfileID != profile.ID || remote.FolderToken != sc.FolderToken || mount.ID == "" || mount.RemoteID != remote.ID {
		return model.ErrIdentityChanged
	}
	return nil
}
func (s *Service) validateCurrent(ctx context.Context) (model.Config, error) {
	c, err := config.LoadWithPaths(s.paths)
	if err != nil {
		return c, err
	}
	known := map[model.MountID]bool{}
	paths := s.paths
	if c.StateDir != "" {
		paths.StateDir = c.StateDir
	}
	if c.CacheDir != "" {
		paths.CacheDir = c.CacheDir
	}
	for _, m := range s.mounts {
		sc, err := m.Store.Scope(ctx)
		if err != nil {
			return c, err
		}
		if known[sc.MountID] {
			return c, errors.New("duplicate mount source")
		}
		known[sc.MountID] = true
		if err = binding(c, sc); err != nil {
			return c, err
		}
		expected, err := paths.MountPaths(sc.MountID)
		if err != nil {
			return c, err
		}
		if filepath.Clean(expected.Root) != filepath.Clean(m.Dir) {
			return c, errors.New("store directory does not match its configured mount namespace")
		}
		if err = m.Store.Validate(ctx); err != nil {
			return c, err
		}
	}
	for _, m := range c.Mounts {
		if known[m.ID] {
			continue
		}
		mp, err := paths.MountPaths(m.ID)
		if err != nil {
			return c, err
		}
		for _, name := range []string{"store.db", "state.db"} {
			if _, err = os.Stat(filepath.Join(mp.Root, name)); err == nil {
				return c, fmt.Errorf("configured store omitted from backup: %s", m.ID)
			} else if !os.IsNotExist(err) {
				return c, err
			}
		}
	}
	return c, nil
}

type UninstallResult struct {
	PreserveData, NeedsStartupHandoff bool
	ConfigFile, StateDir, CacheDir    string
}

// Uninstall prepares the data-preserving lifecycle decision. Installation files
// and startup registration are owned by the manager/packager, not removed here.
func (s *Service) Uninstall(ctx context.Context) (result UninstallResult, err error) {
	err = s.quiesce(ctx, func(ctx context.Context) error {
		if _, err := s.validateCurrent(ctx); err != nil {
			return err
		}
		result = UninstallResult{true, true, s.paths.ConfigFile, s.paths.StateDir, s.paths.CacheDir}
		return nil
	})
	return
}

// Guard implementations must call fn synchronously exactly once and propagate
// errors. Detecting a skipped callback prevents false success from an inert guard.
func (s *Service) quiesce(ctx context.Context, fn func(context.Context) error) error {
	calls := 0
	var operationErr error
	guardErr := s.guard.WithQuiescence(ctx, func(inner context.Context) error {
		calls++
		if calls != 1 {
			return ErrNotQuiescent
		}
		if inner == nil {
			operationErr = ErrNotQuiescent
			return operationErr
		}
		operationErr = fn(inner)
		return operationErr
	})
	if calls != 1 {
		return errors.Join(ErrNotQuiescent, guardErr)
	}
	return errors.Join(guardErr, operationErr)
}
