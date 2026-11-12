package manager

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

// WithInstallationLease excludes every managed worker start and config writer
// in this OS-user domain. It retains historical stores, accepts durable pending
// work, and never clears queues, changes auth or requests a cloud operation.
// fn must not re-enter manager/config/store ownership. Cancellation may abort
// acquisition, but after fn starts its return is the only release boundary.
func (m *Manager) WithInstallationLease(ctx context.Context, fn func() error) error {
	if fn == nil {
		return ErrConfig
	}
	release, err := m.lifecycleLease(ctx)
	if err != nil {
		return err
	}
	defer release()

	// Stop desktop recovery and wait for UI/tray teardown before taking config
	// locks or replacing files. Retain singleton ownership through replacement.
	releaseDesktop, err := m.installationDesktopLease(ctx)
	if err != nil {
		return err
	}
	defer releaseDesktop()

	releases := []func(){}
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	lock := func(path string) error {
		l, err := privateLock(path)
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
		releases = append(releases, func() { _ = l.Unlock() })
		return nil
	}
	configs, err := m.installationConfigs(ctx)
	if err != nil {
		return err
	}
	var owners []ownership
	for _, file := range configs {
		if err = lock(file + ".manager.lock"); err != nil {
			return err
		}
		if err = lock(file + ".lock"); err != nil {
			return err
		}
		other := Manager{options: m.options, domain: m.domain}
		other.options.ConfigFile = file
		old, err := other.owners(ctx)
		if err != nil {
			return err
		}
		if len(old) > 0 {
			if err = installationRegular(file); err != nil {
				return err
			}
		}
		c, err := other.LoadConfig(ctx)
		if err != nil {
			return err
		}
		owners = append(owners, old...)
		for _, mount := range c.Mounts {
			spec, err := other.spec(ctx, mount.ID)
			if err != nil {
				return err
			}
			recorded := false
			for _, owner := range old {
				if owner.Root == spec.Paths.Root {
					recorded = true
					break
				}
			}
			if !recorded {
				// A configuration entry is intent, not evidence of a historical
				// store. The global lifecycle lease prevents its first start here.
				if err = installationUnusedRoot(spec.Paths.Root); err != nil {
					return err
				}
				continue
			}
			owners = append(owners, ownershipFor(file, spec))
		}
	}
	// Order is global lifecycle -> sorted config locks -> sorted run locks ->
	// sorted store locks. The auth global lease makes this inventory stable.
	groups := map[string][]ownership{}
	roots := map[string]ownership{}
	for _, o := range owners {
		if err = validateInstallationOwner(o); err != nil {
			return err
		}
		if old, ok := roots[o.Root]; ok && old.Scope != o.Scope {
			return model.ErrIdentityChanged
		}
		roots[o.Root] = o
		key := o.ConfigFile + "\x00" + string(o.Scope.MountID)
		duplicate := false
		for _, old := range groups[key] {
			if old.Root == o.Root {
				duplicate = true
				break
			}
		}
		if !duplicate {
			groups[key] = append(groups[key], o)
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group, func(i, j int) bool { return group[i].Root < group[j].Root })
		other := Manager{options: m.options, domain: m.domain}
		other.options.ConfigFile = group[0].ConfigFile
		l, err := privateLock(other.runtimeLockPath(group[0].Scope.MountID))
		if err != nil {
			return err
		}
		ok, err := l.TryLock()
		if err != nil {
			return err
		}
		if !ok {
			if err = stopInstallationOwner(ctx, group); err != nil {
				return err
			}
			waitCtx, cancel := context.WithTimeout(ctx, 61*time.Second)
			ok, err = l.TryLockContext(waitCtx, 5*time.Millisecond)
			cancel()
			if err != nil {
				return err
			}
			if !ok {
				return ErrPending
			}
		}
		releases = append(releases, func() { _ = l.Unlock() })
	}
	rootPaths := make([]string, 0, len(roots))
	for root := range roots {
		rootPaths = append(rootPaths, root)
	}
	sort.Strings(rootPaths)
	for _, root := range rootPaths {
		o := roots[root]
		// Do not create/rebind a missing root and do not use Store.Open, which writes
		// metadata even for an existing database. Hold its real writer ownership lock
		// and audit the existing database read-only instead.
		if err = installationRegular(filepath.Join(root, "store.db")); err != nil {
			return err
		}
		if err = lock(filepath.Join(root, "worker.lock")); err != nil {
			return err
		}
		if pending, err := offlineCachePending(o); err != nil || pending {
			return errors.Join(ErrPending, err)
		}
		if err = store.ValidateDirectory(ctx, root, o.Scope); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// Only an absent or strictly empty plain root can represent an unstarted mount.
// Never adopt an occupied directory, even if it contains an otherwise valid DB;
// persisted historical owners always take the full store audit path instead.
func installationUnusedRoot(root string) error {
	if err := platform.CheckNoSymlinks(root); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrConfig
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w: configured mount has unaccounted data", ErrConfig)
	}
	return nil
}

func installationRegular(path string) error {
	if err := platform.CheckNoSymlinks(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return errors.Join(ErrConfig, err)
	}
	if !info.Mode().IsRegular() {
		return ErrConfig
	}
	return nil
}
func validateInstallationOwner(o ownership) error {
	if o.SchemaVersion != 1 || !filepath.IsAbs(o.ConfigFile) || filepath.Clean(o.ConfigFile) != o.ConfigFile || !validID(string(o.Scope.MountID)) || !validID(string(o.Scope.ProfileID)) || !validID(string(o.Scope.RemoteID)) || o.Scope.IdentityHash == "" || o.Scope.FolderToken == "" || !filepath.IsAbs(o.Root) || filepath.Clean(o.Root) != o.Root || filepath.Base(o.Root) != string(o.Scope.MountID) || filepath.Base(filepath.Dir(o.Root)) != "mounts" || o.VFSRoot != filepath.Join(o.Root, "vfs") {
		return ErrConfig
	}
	if err := platform.CheckNoSymlinks(o.Root); err != nil {
		return err
	}
	if err := platform.CheckNoSymlinks(o.VFSRoot); err != nil {
		return err
	}
	return nil
}

type configReference struct {
	Schema     int    `json:"schema"`
	ConfigFile string `json:"config_file"`
}

// A retired owner domain retains its exact config identity, not just lock files.
// This allows future installation without guessing a path from a config hash.
func installationConfigReference(dir string) (string, error) {
	name := filepath.Join(dir, "config.ref")
	if err := installationRegular(name); err != nil {
		return "", err
	}
	info, err := os.Stat(name)
	if err != nil {
		return "", err
	}
	if info.Size() > 64<<10 {
		return "", ErrConfig
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	var ref configReference
	if json.Unmarshal(b, &ref) != nil || ref.Schema != 1 || !filepath.IsAbs(ref.ConfigFile) || filepath.Clean(ref.ConfigFile) != ref.ConfigFile || hashPath(ref.ConfigFile) != filepath.Base(dir) {
		return "", ErrConfig
	}
	return ref.ConfigFile, nil
}

// Discover historical owners and explicitly retired config domains. A lock-only
// directory without either kind of identity evidence is still refused.
func (m *Manager) installationConfigs(ctx context.Context) ([]string, error) {
	files := map[string]bool{m.options.ConfigFile: true}
	root := filepath.Join(m.domain, "worker-state")
	if err := platform.CheckNoSymlinks(root); err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		decoded, err := hex.DecodeString(dir.Name())
		if err != nil || len(decoded) != 32 || strings.ToLower(dir.Name()) != dir.Name() || !dir.IsDir() || dir.Type()&os.ModeSymlink != 0 {
			return nil, ErrConfig
		}
		entries, err := os.ReadDir(filepath.Join(root, dir.Name()))
		if err != nil {
			return nil, err
		}
		found := false
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(root, dir.Name(), entry.Name())
			if err = installationRegular(path); err != nil {
				return nil, err
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() > 64<<10 {
				return nil, ErrConfig
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			var o ownership
			if json.Unmarshal(raw, &o) != nil || validateInstallationOwner(o) != nil || hashPath(o.ConfigFile) != dir.Name() || entry.Name() != string(o.Scope.MountID)+"-"+hashPath(o.Root)+".json" {
				return nil, ErrConfig
			}
			files[o.ConfigFile] = true
			found = true
		}
		if !found && len(entries) > 0 {
			file, err := installationConfigReference(filepath.Join(root, dir.Name()))
			if err != nil {
				return nil, fmt.Errorf("%w: owner directory has no valid root reference: %v", ErrConfig, err)
			}
			files[file] = true
		}
	}
	out := make([]string, 0, len(files))
	for file := range files {
		out = append(out, file)
	}
	sort.Strings(out)
	return out, nil
}

// A runtime lock can cover several historical roots. Authenticate every usable
// endpoint first; an old dead endpoint is not evidence that the active worker
// stopped. Multiple live identities or an unreachable locked owner are refused.
func stopInstallationOwner(ctx context.Context, owners []ownership) error {
	type live struct {
		ep  endpoint
		key string
	}
	var active *live
	for _, o := range owners {
		ep, key, err := readEndpointAt(o.Root, o.Scope.MountID, o.ConfigFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errors.Join(ErrPending, err)
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		var state model.MountStatus
		err = callEndpoint(probeCtx, ep, key, "/status", nil, &state)
		cancel()
		if err != nil {
			if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInstanceChanged) {
				continue
			}
			return errors.Join(ErrPending, err)
		}
		if active != nil && (active.ep.InstanceID != ep.InstanceID || active.ep.Address != ep.Address) {
			return ErrInstanceChanged
		}
		active = &live{ep, key}
	}
	if active == nil {
		return ErrPending
	}
	var out struct{}
	err := callEndpoint(ctx, active.ep, active.key, "/recovery/stop", map[string]int64{"wait_ns": int64(60 * time.Second)}, &out)
	// A stop response can race server teardown. The held run-lock check by the
	// caller, never PID existence, determines whether shutdown actually completed.
	if errors.Is(err, ErrUnavailable) {
		return nil
	}
	return err
}
