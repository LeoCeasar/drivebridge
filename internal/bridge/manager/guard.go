package manager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/file"
	"github.com/rclone/rclone/vfs/vfscache"
)

type ownership struct {
	SchemaVersion int         `json:"schema_version"`
	ConfigFile    string      `json:"config_file"`
	Scope         store.Scope `json:"scope"`
	Root, VFSRoot string
}

func hashPath(p string) string {
	h := sha256.Sum256([]byte(filepath.Clean(p)))
	return hex.EncodeToString(h[:])
}
func (m *Manager) ownerDir() string {
	return filepath.Join(m.domain, "worker-state", hashPath(m.options.ConfigFile))
}
func (m *Manager) runtimeLockPath(id model.MountID) string {
	return filepath.Join(m.ownerDir(), string(id)+".lock")
}
func ownershipFor(file string, s mountSpec) ownership {
	return ownership{SchemaVersion: 1, ConfigFile: file, Scope: store.Scope{MountID: s.Mount.ID, ProfileID: s.Profile.ID, RemoteID: s.Remote.ID, IdentityHash: s.Profile.IdentityHash, FolderToken: s.Remote.FolderToken}, Root: s.Paths.Root, VFSRoot: s.Paths.VFS}
}
func (m *Manager) recordOwner(s mountSpec) error {
	if s.Profile.IdentityHash == "" {
		return auth.ErrUserMissing
	}
	o := ownershipFor(m.options.ConfigFile, s)
	file := filepath.Join(m.ownerDir(), string(s.Mount.ID)+"-"+hashPath(o.Root)+".json")
	return atomicJSON(file, o)
}
func (m *Manager) owners(ctx context.Context) ([]ownership, error) {
	entries, err := os.ReadDir(m.ownerDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []ownership{}
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		file := filepath.Join(m.ownerDir(), e.Name())
		if err = platform.CheckNoSymlinks(file); err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var o ownership
		if len(raw) > 64<<10 || json.Unmarshal(raw, &o) != nil || o.SchemaVersion != 1 || o.ConfigFile != m.options.ConfigFile || !validID(string(o.Scope.MountID)) || !validID(string(o.Scope.ProfileID)) || !validID(string(o.Scope.RemoteID)) || o.Scope.IdentityHash == "" || o.Scope.FolderToken == "" || !filepath.IsAbs(o.Root) || !filepath.IsAbs(o.VFSRoot) || filepath.Base(o.Root) != string(o.Scope.MountID) || filepath.Base(filepath.Dir(o.Root)) != "mounts" || o.VFSRoot != filepath.Join(o.Root, "vfs") {
			return nil, ErrConfig
		}
		if e.Name() != string(o.Scope.MountID)+"-"+hashPath(o.Root)+".json" {
			return nil, ErrConfig
		}
		out = append(out, o)
	}
	return out, nil
}
func offlineCachePending(o ownership) (bool, error) {
	path := filepath.Join(o.Root, "cache-manifest.json")
	if err := platform.CheckNoSymlinks(path); err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return unmanifestedCachePending(o.VFSRoot)
	}
	if err != nil {
		return false, err
	}
	var manifest cacheManifest
	if json.Unmarshal(raw, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.MountID != o.Scope.MountID {
		return false, ErrConfig
	}
	if manifest.DataRoot == "" && manifest.MetaRoot == "" {
		return unmanifestedCachePending(o.VFSRoot)
	}
	for _, p := range []string{manifest.DataRoot, manifest.MetaRoot} {
		relative, err := filepath.Rel(file.UNCPath(o.VFSRoot), file.UNCPath(p))
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || !filepath.IsAbs(p) {
			return false, ErrConfig
		}
		if err = platform.CheckNoSymlinks(p); err != nil {
			return false, err
		}
	}
	return vfscache.ManagedMetadataPending(manifest.DataRoot, manifest.MetaRoot)
}

// A native mount can fail after VFS initialization has created an empty cache
// directory tree, before it has published a cache manifest. Such directories
// contain no pending bytes. Every file (including empty files), unsafe path or
// unreadable directory still prevents replacement. Nothing is removed or adopted.
func unmanifestedCachePending(root string) (bool, error) {
	pending := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if err := platform.CheckNoSymlinks(path); err != nil {
			return err
		}
		if !entry.IsDir() {
			pending = true
			return filepath.SkipAll
		}
		return nil
	})
	return pending, err
}
func durablePending(ctx context.Context, s *store.Store) (bool, error) {
	jobs, err := s.Pending(ctx)
	if err != nil {
		return false, err
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil {
		return false, err
	}
	creates, err := s.PendingCreates(ctx)
	if err != nil {
		return false, err
	}
	names, err := s.PendingNamespace(ctx)
	if err != nil {
		return false, err
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil {
		return false, err
	}
	pubs, err := s.PendingPublications(ctx)
	if err != nil {
		return false, err
	}
	return len(jobs)+len(conflicts)+len(creates)+len(names)+len(actions)+len(pubs) > 0, nil
}

// Acquire is called while auth.Service already holds the OS-user exclusive
// lifecycle lease. New parent/hidden-worker starts share that same lease. A live
// mount is busy; offline ownership locks remain held through the auth operation.
func (m *Manager) Acquire(ctx context.Context, appID string, refs []auth.Reference) (func(), error) {
	return m.acquireRefs(ctx, refs, false)
}
func (m *Manager) acquireRefs(ctx context.Context, refs []auth.Reference, allowPending bool) (func(), error) {
	if len(refs) == 0 {
		return func() {}, nil
	}
	releases := []func(){}
	cleanup := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()
	seen := map[string]bool{}
	seenRoots := map[string]store.Scope{}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ConfigFile < refs[j].ConfigFile })
	for _, ref := range refs {
		if !filepath.IsAbs(ref.ConfigFile) || ref.ConfigID != hashPath(ref.ConfigFile) {
			return nil, auth.ErrReferencesBusy
		}
		if err := platform.CheckNoSymlinks(ref.ConfigFile); err != nil {
			return nil, auth.ErrReferencesBusy
		}
		info, err := os.Stat(ref.ConfigFile)
		if err != nil || !info.Mode().IsRegular() {
			return nil, auth.ErrReferencesBusy
		}
		c, err := config.Load(ref.ConfigFile)
		if err != nil {
			return nil, auth.ErrReferencesBusy
		}
		var p model.Profile
		for _, v := range c.Profiles {
			if v.ID == ref.ProfileID {
				p = v
			}
		}
		if p.ID == "" || p.AuthRef != ref.AuthRef || p.IdentityHash != ref.IdentityHash {
			return nil, auth.ErrReferencesBusy
		}
		other := Manager{options: m.options, domain: m.domain}
		other.options.ConfigFile = ref.ConfigFile
		owners, err := other.owners(ctx)
		if err != nil {
			return nil, auth.ErrReferencesBusy
		}
		for _, mount := range c.Mounts {
			var remote model.Remote
			for _, r := range c.Remotes {
				if r.ID == mount.RemoteID {
					remote = r
				}
			}
			if remote.ProfileID != ref.ProfileID {
				continue
			}
			spec, err := other.spec(ctx, mount.ID)
			if err != nil {
				return nil, auth.ErrReferencesBusy
			}
			owners = append(owners, ownershipFor(ref.ConfigFile, spec))
		}
		for _, o := range owners {
			if o.Scope.ProfileID != ref.ProfileID {
				continue
			}
			if o.Scope.IdentityHash != ref.IdentityHash {
				return nil, auth.ErrReferencesBusy
			}
			rootKey := ref.ConfigID + "/" + o.Root
			if old, ok := seenRoots[rootKey]; ok {
				if old != o.Scope {
					return nil, auth.ErrReferencesBusy
				}
				continue
			}
			seenRoots[rootKey] = o.Scope
			key := ref.ConfigID + "/" + string(o.Scope.MountID)
			if !seen[key] {
				l, err := privateLock(other.runtimeLockPath(o.Scope.MountID))
				if err != nil {
					return nil, auth.ErrReferencesBusy
				}
				ok, err := l.TryLock()
				if err != nil || !ok {
					return nil, auth.ErrReferencesBusy
				}
				releases = append(releases, func() { _ = l.Unlock() })
				seen[key] = true
			}
			if pending, err := offlineCachePending(o); err != nil || (pending && !allowPending) {
				return nil, auth.ErrReferencesBusy
			}
			if _, err = os.Stat(filepath.Join(o.Root, "store.db")); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return nil, auth.ErrReferencesBusy
			}
			if err = store.ValidateDirectory(ctx, o.Root, o.Scope); err != nil {
				return nil, auth.ErrReferencesBusy
			}
			s, err := store.Open(o.Root)
			if err != nil {
				return nil, auth.ErrReferencesBusy
			}
			releases = append(releases, func() { _ = s.Close() })
			if err = s.BindScope(ctx, o.Scope); err != nil {
				return nil, auth.ErrReferencesBusy
			}
			if pending, err := durablePending(ctx, s); err != nil || (pending && !allowPending) {
				return nil, auth.ErrReferencesBusy
			}
		}
	}
	success = true
	return cleanup, nil
}

var _ auth.LifecycleGuard = (*Manager)(nil)

// AcquireRecovery is used only by explicit same-identity account recovery. The
// auth service holds exclusive lifecycle admission before invoking it.
func (m *Manager) AcquireRecovery(ctx context.Context, appID string, refs []auth.Reference) (func(), error) {
	seen := map[string]bool{}
	for _, ref := range refs {
		other := Manager{options: m.options, domain: m.domain}
		other.options.ConfigFile = ref.ConfigFile
		c, err := other.LoadConfig(ctx)
		if err != nil {
			return nil, auth.ErrReferencesBusy
		}
		owners, err := other.owners(ctx)
		if err != nil {
			return nil, auth.ErrReferencesBusy
		}
		for _, mount := range c.Mounts {
			s, err := other.spec(ctx, mount.ID)
			if err != nil {
				return nil, auth.ErrReferencesBusy
			}
			if s.Profile.ID == ref.ProfileID {
				owners = append(owners, ownershipFor(ref.ConfigFile, s))
			}
		}
		for _, o := range owners {
			if o.Scope.ProfileID != ref.ProfileID {
				continue
			}
			key := ref.ConfigID + "/" + string(o.Scope.MountID)
			if seen[key] {
				continue
			}
			seen[key] = true
			l, err := privateLock(other.runtimeLockPath(o.Scope.MountID))
			if err != nil {
				return nil, auth.ErrReferencesBusy
			}
			ok, err := l.TryLock()
			if err != nil {
				return nil, auth.ErrReferencesBusy
			}
			if ok {
				_ = l.Unlock()
				continue
			}
			if err = other.stopForRecovery(ctx, o.Scope.MountID, 60*time.Second); err != nil {
				return nil, auth.ErrReferencesBusy
			}
		}
	}
	return m.acquireRefs(ctx, refs, true)
}

func (m *Manager) validateOwnership(ctx context.Context, spec mountSpec) error {
	owners, err := m.owners(ctx)
	if err != nil {
		return err
	}
	expected := ownershipFor(m.options.ConfigFile, spec)
	for _, old := range owners {
		if old.Scope.MountID != spec.Mount.ID {
			continue
		}
		if old.Root == expected.Root && old.Scope == expected.Scope {
			continue
		}
		if _, err := os.Stat(filepath.Join(old.Root, "store.db")); err == nil {
			return model.ErrIdentityChanged
		} else if !os.IsNotExist(err) {
			return err
		}
		if pending, err := offlineCachePending(old); err != nil || pending {
			return model.ErrIdentityChanged
		}
	}
	return nil
}
