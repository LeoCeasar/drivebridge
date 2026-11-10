package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

type Paths struct{ ConfigFile, StateDir, CacheDir string }
type MountPaths struct{ Root, Database, Blobs, VFS, Logs, Conflicts, Cache string }

func DefaultPaths() (Paths, error) {
	conf, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return Paths{}, err
	}
	state, err := platform.StateDir()
	if err != nil {
		return Paths{}, err
	}
	p := Paths{filepath.Join(conf, "DriveBridge", "config.json"), state, filepath.Join(cache, "DriveBridge", "Cache")}
	return p, p.validate()
}
func validUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && strings.EqualFold(parsed.String(), id)
}
func (p Paths) validate() error {
	for _, v := range []string{p.ConfigFile, p.StateDir, p.CacheDir} {
		if !filepath.IsAbs(v) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("absolute nonempty paths required: %q", v)
		}
	}
	return nil
}
func (p Paths) ProfileDir(id model.ProfileID) (string, error) {
	if !validUUID(string(id)) {
		return "", fmt.Errorf("invalid profile UUID")
	}
	if err := p.validate(); err != nil {
		return "", err
	}
	return filepath.Join(p.StateDir, "profiles", string(id)), nil
}
func (p Paths) MountPaths(id model.MountID) (MountPaths, error) {
	if !validUUID(string(id)) {
		return MountPaths{}, fmt.Errorf("invalid mount UUID")
	}
	if err := p.validate(); err != nil {
		return MountPaths{}, err
	}
	root := filepath.Join(p.StateDir, "mounts", string(id))
	return MountPaths{root, filepath.Join(root, "state.db"), filepath.Join(root, "blobs"), filepath.Join(root, "vfs"), filepath.Join(root, "logs"), filepath.Join(root, "conflicts"), filepath.Join(p.CacheDir, string(id))}, nil
}
func effectivePaths(p Paths, c Config) Paths {
	if c.StateDir != "" {
		p.StateDir = c.StateDir
	}
	if c.CacheDir != "" {
		p.CacheDir = c.CacheDir
	}
	return p
}
func validAuthRef(ref string) bool {
	parts := strings.Split(ref, "/")
	return len(parts) == 4 && parts[0] == "auth" && parts[1] == "identities" && validUUID(parts[2]) && parts[3] == "lark"
}

// ResolveAuthRoot resolves the legacy state-local registry only. New managed
// workers MUST use auth.Service.Resolve, whose OS-user domain is independent of
// StateDir and unifies cross-config credential locks/references. This legacy
// helper does not enroll or migrate credentials and is not a production binding.
// Deprecated: use auth.Service.Resolve for managed profiles.
func ResolveAuthRoot(p Paths, r model.IdentityRegistry, profile model.Profile) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if err := ValidateIdentityRegistry(r); err != nil {
		return "", err
	}
	for _, record := range r.Identities {
		if record.AuthRef == profile.AuthRef && record.IdentityHash == profile.IdentityHash && record.Brand == profile.Brand {
			path := filepath.Join(p.StateDir, filepath.FromSlash(record.AuthRef))
			if err := platform.CheckNoSymlinks(path); err != nil {
				return "", err
			}
			return path, nil
		}
	}
	return "", model.ErrIdentityChanged
}
