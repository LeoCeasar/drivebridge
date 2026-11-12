// Package config reads and atomically persists credential-free product configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

const SchemaVersion = 1

// DefaultCacheMaxAge releases unused, safely synced bodies after seven days.
const DefaultCacheMaxAge = "168h"

// BridgeConfigFlag is distinct from rclone's existing --config flag.
const BridgeConfigFlag = "bridge-config"

var ErrUnknownSchema = errors.New("unsupported config schema: read only")

type Config = model.Config
type Profile = model.Profile
type Remote = model.Remote
type Mount = model.Mount

func DefaultMount() Mount {
	return Mount{BackgroundPullExcludedExtensions: []string{".log", ".db"}, CacheMode: "full", CacheMaxBytes: 5368709120, CacheMaxAge: DefaultCacheMaxAge, WriteBack: "2s", PollDocs: "5s", RefreshDirs: "30s", MaxDocBytes: 10485760, Notify: true, MountEngine: "auto"}
}

// EffectiveCacheMaxAge keeps old configurations compatible. Zero disables age
// eviction and leaves the clean-body capacity policy active.
func EffectiveCacheMaxAge(mount Mount) (time.Duration, error) {
	value := mount.CacheMaxAge
	if value == "" {
		value = DefaultCacheMaxAge
	}
	age, err := time.ParseDuration(value)
	if err != nil || age < 0 {
		return 0, fmt.Errorf("invalid cache max age %q", value)
	}
	return age, nil
}

func Default() Config {
	return Config{SchemaVersion: SchemaVersion, Profiles: []Profile{}, Remotes: []Remote{}, Mounts: []Mount{}}
}

func pathsForFile(path string) (Paths, error) {
	p, err := DefaultPaths()
	if err != nil {
		return Paths{}, err
	}
	if path != "" {
		p.ConfigFile, err = filepath.Abs(path)
	}
	return p, err
}
func Load(path string) (Config, error) {
	p, err := pathsForFile(path)
	if err != nil {
		return Config{}, err
	}
	return LoadWithPaths(p)
}
func Save(path string, c Config) error {
	p, err := pathsForFile(path)
	if err != nil {
		return err
	}
	return SaveWithPaths(p, c)
}

// LoadWithPaths returns an empty schema-1 configuration for a missing file.
// Unknown schemas are returned for inspection with ErrUnknownSchema and cannot be saved.
func LoadWithPaths(p Paths) (Config, error) {
	if err := p.validate(); err != nil {
		return Config{}, err
	}
	if err := platform.CheckNoSymlinks(p.ConfigFile); err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(p.ConfigFile)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return c, err
	}
	if header.SchemaVersion != SchemaVersion {
		c.SchemaVersion = header.SchemaVersion
		return c, ErrUnknownSchema
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("trailing configuration data")
	}
	return c, Validate(c, p)
}

// SaveWithPaths serializes concurrent writers with a file lock, checks disk schema
// under that lock, fsyncs a private temporary file, replaces, then syncs its directory.
// A directory-sync error may mean the replacement is visible but durability is uncertain.
func SaveWithPaths(p Paths, c Config) error {
	if err := Validate(c, p); err != nil {
		return err
	}
	dir := filepath.Dir(p.ConfigFile)
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return err
	}
	lockPath := p.ConfigFile + ".lock"
	if err := platform.CheckNoSymlinks(lockPath); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err := lockFile.Close(); err != nil {
		return err
	}
	if err := platform.SecurePrivate(lockPath, false); err != nil {
		return err
	}
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	// Refuse to destroy both future and malformed existing configuration.
	if _, err := LoadWithPaths(p); err != nil {
		return err
	}
	resolved := effectivePaths(p, c)
	for _, root := range []string{resolved.StateDir, resolved.CacheDir} {
		if err := claimPrivateRoot(root, p.ConfigFile); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	defer func() { _ = os.Remove(temporary) }()
	if err = platform.SecurePrivate(temporary, false); err == nil {
		_, err = f.Write(data)
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
	if err = platform.ReplaceFile(temporary, p.ConfigFile); err != nil {
		return err
	}
	return platform.SyncDir(dir)
}

// Validate checks referential integrity and paths without creating directories.
// Overrides are private to a configuration; ownership is checked when saved.
func Validate(c Config, p Paths) error {
	if c.SchemaVersion != SchemaVersion {
		return ErrUnknownSchema
	}
	p = effectivePaths(p, c)
	if err := p.validate(); err != nil {
		return err
	}
	ids := map[string]bool{}
	profiles := map[model.ProfileID]bool{}
	remotes := map[model.RemoteID]bool{}
	claim := func(id string) error {
		key := strings.ToLower(id)
		if !validUUID(id) || ids[key] {
			return fmt.Errorf("invalid or duplicate UUID %q", id)
		}
		ids[key] = true
		return nil
	}
	identities := map[string]string{}
	refs := map[string]string{}
	for _, profile := range c.Profiles {
		if profile.History != nil && !profile.History.Valid() {
			return errors.New("invalid history retention policy")
		}
		if err := claim(string(profile.ID)); err != nil {
			return err
		}
		profiles[profile.ID] = true
		if strings.TrimSpace(profile.Name) == "" || (profile.Brand != "feishu" && profile.Brand != "lark") {
			return fmt.Errorf("invalid profile name or brand")
		}
		if (profile.AuthRef == "") != (profile.IdentityHash == "") {
			return fmt.Errorf("incomplete identity reference")
		}
		if profile.AuthRef != "" {
			if !validAuthRef(profile.AuthRef) {
				return fmt.Errorf("invalid auth_ref")
			}
			if previous, ok := identities[profile.IdentityHash]; ok && previous != profile.AuthRef {
				return fmt.Errorf("identity has multiple auth roots")
			}
			identities[profile.IdentityHash] = profile.AuthRef
			if previous, ok := refs[profile.AuthRef]; ok && previous != profile.IdentityHash {
				return fmt.Errorf("auth root has multiple identities")
			}
			refs[profile.AuthRef] = profile.IdentityHash
		}
	}
	accountRoots := map[model.ProfileID]bool{}
	accountRemotes := map[model.RemoteID]bool{}
	for _, remote := range c.Remotes {
		if err := claim(string(remote.ID)); err != nil {
			return err
		}
		remotes[remote.ID] = true
		if !profiles[remote.ProfileID] || strings.TrimSpace(remote.Name) == "" || strings.TrimSpace(remote.FolderToken) == "" || remote.NewMDPolicy != "docx" {
			return fmt.Errorf("invalid remote or missing profile")
		}
		if remote.AccountRoot {
			if remote.FolderToken != "drivebridge-account-"+string(remote.ProfileID) || accountRoots[remote.ProfileID] {
				return fmt.Errorf("invalid or duplicate account directory root")
			}
			accountRoots[remote.ProfileID] = true
			accountRemotes[remote.ID] = true
		} else if strings.HasPrefix(remote.FolderToken, "drivebridge-account-") {
			return fmt.Errorf("reserved account directory token")
		}
		if remote.MountRemoved && !remote.AccountRoot {
			return fmt.Errorf("only account roots have a mount removal intent")
		}
	}
	privatePaths := []string{filepath.Dir(p.ConfigFile), p.StateDir, p.CacheDir}
	for _, path := range privatePaths {
		if err := platform.CheckNoSymlinks(path); err != nil {
			return err
		}
	}
	if overlaps(p.StateDir, p.CacheDir) {
		return fmt.Errorf("state and cache directories overlap")
	}
	mounts := []string{}
	mountedAccounts := map[model.RemoteID]bool{}
	for _, mount := range c.Mounts {
		if err := claim(string(mount.ID)); err != nil {
			return err
		}
		if !remotes[mount.RemoteID] || strings.TrimSpace(mount.Name) == "" {
			return fmt.Errorf("invalid mount or missing remote")
		}
		if accountRemotes[mount.RemoteID] {
			if mountedAccounts[mount.RemoteID] {
				return fmt.Errorf("account directory already has a mount")
			}
			mountedAccounts[mount.RemoteID] = true
		}
		path := mount.Mountpoint
		if !platform.ValidMountpoint(path) {
			return fmt.Errorf("invalid mountpoint %q", path)
		}
		// A bare Windows drive and its root are the same mount namespace.
		if filepath.Separator == '\\' && len(path) == 2 && path[1] == ':' {
			path += string(filepath.Separator)
		}
		// Mountpoints are runtime targets, not private-storage paths. Never stat
		// them while loading config: a mount request can reload this config for
		// identity checks, and probing its own root would reenter the filesystem.
		// Startup checks the selected target's availability and Unix symlinks.
		for _, other := range append(privatePaths, mounts...) {
			if overlaps(path, other) {
				return fmt.Errorf("mountpoint overlaps mount or private directory")
			}
		}
		mounts = append(mounts, path)
		if mount.CacheMode != "full" || mount.CacheMaxBytes <= 0 || mount.MaxDocBytes <= 0 || mount.MaxDocBytes > 10*1024*1024 {
			return fmt.Errorf("invalid mount cache or document limit")
		}
		if _, err := EffectiveCacheMaxAge(mount); err != nil {
			return err
		}
		if _, err := EffectiveBackgroundPullExcludedExtensions(mount); err != nil {
			return err
		}
		if mount.MountEngine != "auto" {
			return fmt.Errorf("invalid mount engine")
		}
		switch mount.MacOSBackend {
		case "": // Preserve the platform's existing default.
		case "kernel", "fskit":
			if runtime.GOOS != "darwin" {
				return fmt.Errorf("macos backend requires macOS")
			}
		default:
			return fmt.Errorf("invalid macos backend")
		}
		for _, value := range []string{mount.WriteBack, mount.PollDocs, mount.RefreshDirs} {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				return fmt.Errorf("invalid positive duration %q", value)
			}
		}
	}
	return nil
}
func overlaps(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if filepath.Separator == '\\' {
		a = strings.ToLower(a)
		b = strings.ToLower(b)
	}
	rel, err := filepath.Rel(a, b)
	if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return true
	}
	rel, err = filepath.Rel(b, a)
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ValidateIdentityRegistry enforces one canonical CLI auth root per (AppID, UserOpenID).
// Brand is checked by product identity binding and never partitions credential locks.
func ValidateIdentityRegistry(r model.IdentityRegistry) error {
	if r.SchemaVersion != SchemaVersion {
		return ErrUnknownSchema
	}
	ids := map[string]bool{}
	keys := map[[2]string]string{}
	roots := map[string][2]string{}
	hashes := map[string][2]string{}
	for _, record := range r.Identities {
		key := [2]string{record.AppID, record.UserOpenID}
		if !validUUID(record.ID) || ids[strings.ToLower(record.ID)] || record.AuthRef != "auth/identities/"+record.ID+"/lark" || !validAuthRef(record.AuthRef) || record.AppID == "" || record.UserOpenID == "" || record.IdentityHash == "" || (record.Brand != "feishu" && record.Brand != "lark") {
			return fmt.Errorf("invalid identity registry record")
		}
		ids[strings.ToLower(record.ID)] = true
		if _, ok := keys[key]; ok {
			return fmt.Errorf("duplicate identity must reuse its existing record")
		}
		keys[key] = record.AuthRef
		if previous, ok := roots[record.AuthRef]; ok && previous != key {
			return fmt.Errorf("auth root shared by different identities")
		}
		roots[record.AuthRef] = key
		if previous, ok := hashes[record.IdentityHash]; ok && previous != key {
			return fmt.Errorf("identity hash shared by different identities")
		}
		hashes[record.IdentityHash] = key
	}
	return nil
}

// claimPrivateRoot prevents two configuration files from sharing mutable state or cache.
// A previously unowned, nonempty directory is never adopted.
func claimPrivateRoot(root, configFile string) error {
	if err := platform.CheckNoSymlinks(root); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		if err := platform.EnsurePrivateDir(root); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("private root is not a directory: %s", root)
	}
	lockPath := filepath.Join(root, ".drivebridge.lock")
	if err := platform.CheckNoSymlinks(lockPath); err != nil {
		return err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = platform.SecurePrivate(lockPath, false); err != nil {
		return err
	}
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	marker := filepath.Join(root, ".drivebridge-owner")
	if err := platform.CheckNoSymlinks(marker); err != nil {
		return err
	}
	owner := fmt.Sprintf("%x\n", sha256.Sum256([]byte(filepath.Clean(configFile))))
	data, err := os.ReadFile(marker)
	if err == nil {
		if string(data) != owner {
			return fmt.Errorf("private directory belongs to another configuration: %s", root)
		}
		return platform.EnsurePrivateDir(root)
	}
	if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != ".drivebridge.lock" {
			return fmt.Errorf("refusing occupied private directory: %s", root)
		}
	}
	if err := platform.EnsurePrivateDir(root); err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".owner-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if err = platform.SecurePrivate(name, false); err == nil {
		_, err = f.WriteString(owner)
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
	if err = platform.ReplaceFile(name, marker); err != nil {
		return err
	}
	return platform.SyncDir(root)
}
