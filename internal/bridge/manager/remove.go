package manager

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

var ErrMountUnsynced = errors.New("mount has unsynchronized local work")
var ErrHistoryPending = errors.New("mount history is not archived yet")
var ErrCleanupPending = errors.New("mount deleted; cache cleanup will retry")

type removalRecord struct {
	Schema            int           `json:"schema"`
	Mount             model.MountID `json:"mount"`
	Owners            []ownership   `json:"owners"`
	Cache, Mountpoint string
}

func (m *Manager) removalPath(id model.MountID) string {
	return filepath.Join(m.ownerDir(), "removals", string(id)+".json")
}

// DeleteMount removes a clean local mapping and its owned caches. Cloud files
// and the account history repository are outside every deletion path.
func (m *Manager) DeleteMount(ctx context.Context, id model.MountID) error {
	if !validID(string(id)) {
		return ErrConfig
	}
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	intent, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer intent()
	mount, err := m.ResolveMount(ctx, string(id))
	if err != nil {
		if !errors.Is(err, ErrConfig) {
			return err
		}
		// Config deletion can succeed before cleanup completes. Retry its durable
		// record without recreating the mapping or touching the cloud.
		return m.finishRemoval(ctx, id)
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		return err
	}
	// Old OS metadata jobs are local-only under the native product policy.
	// Reconcile their complete journals before deciding that cloud work remains.
	var settled int
	err = m.withStore(ctx, id, "/local-metadata/reconcile", struct{}{}, &settled, func(s *store.Store) error { var e error; settled, e = s.ReconcileSystemMetadata(ctx); return e })
	if err != nil && !errors.Is(err, errNoState) {
		return err
	}
	state, err := m.Status(ctx, id)
	if err != nil {
		return err
	}
	if state.Queued > 0 || state.Inflight > 0 || state.Conflicts > 0 || state.VFSPending > 0 {
		return ErrMountUnsynced
	}
	if state.Running && state.History != nil && state.History.Pending > 50 {
		return ErrHistoryPending
	}
	if !mount.Archived {
		if err := m.stopForRecovery(ctx, id, m.options.ReadyTimeout); err != nil {
			return errors.Join(ErrPending, err)
		}
	}
	lock, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		return err
	}
	ok, err := lock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPending
	}
	defer lock.Unlock()
	owners, err := m.owners(ctx)
	if err != nil {
		return err
	}
	record := removalRecord{Schema: 1, Mount: id, Cache: spec.Paths.Cache, Mountpoint: mount.Mountpoint}
	seen := map[string]bool{}
	for _, o := range append(owners, ownershipFor(m.options.ConfigFile, spec)) {
		if o.Scope.MountID != id || seen[o.Root] {
			continue
		}
		if o.Scope.ProfileID != spec.Profile.ID || o.Scope.IdentityHash != spec.Profile.IdentityHash || o.Scope.RemoteID != spec.Remote.ID {
			return model.ErrIdentityChanged
		}
		if err := validateInstallationOwner(o); err != nil {
			return err
		}
		seen[o.Root] = true
		if err := m.auditRemoval(ctx, o); err != nil {
			return err
		}
		record.Owners = append(record.Owners, o)
	}
	if err := platform.CheckNoSymlinks(record.Cache); err != nil {
		return err
	}
	desktop, err := m.readDesktopState()
	if err != nil {
		return err
	}
	if desktop == nil && mount.Autostart {
		if err := platform.SetStartup(ctx, platform.StartupSpec{ID: string(id), Executable: m.options.Executable}, false); err != nil {
			return err
		}
	}
	// Journal before the config change. On restart the desktop retries cleanup;
	// records whose mapping still exists are deliberately left alone.
	if err := atomicJSON(m.removalPath(id), record); err != nil {
		return err
	}
	disabled := false
	if _, err := m.desktopIntent(ctx, id, &disabled, false); err != nil {
		return err
	}
	if err := m.updateConfig(ctx, func(c *config.Config) error {
		for i, current := range c.Mounts {
			if current.ID != id {
				continue
			}
			if current.RemoteID != mount.RemoteID || current.Mountpoint != mount.Mountpoint {
				return model.ErrVersionChanged
			}
			c.Mounts = append(c.Mounts[:i], c.Mounts[i+1:]...)
			for j := range c.Remotes {
				if c.Remotes[j].ID == mount.RemoteID && c.Remotes[j].AccountRoot {
					c.Remotes[j].MountRemoved = true
				}
			}
			return nil
		}
		return ErrConfig
	}); err != nil {
		return err
	}
	if err := m.finishRemovalRecord(ctx, record); err != nil {
		return errors.Join(ErrCleanupPending, err)
	}
	return nil
}

func (m *Manager) auditRemoval(ctx context.Context, o ownership) error {
	if err := platform.CheckNoSymlinks(o.Root); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(o.Root, "store.db")); os.IsNotExist(err) {
		return auditUninitializedRemoval(o)
	} else if err != nil {
		return err
	}
	if pending, err := offlineCachePending(o); err != nil || pending {
		return errors.Join(ErrMountUnsynced, err)
	}
	s, err := store.Open(o.Root)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.BindScope(ctx, o.Scope); err != nil {
		return err
	}
	jobs, err := s.Pending(ctx)
	if err != nil {
		return err
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil {
		return err
	}
	namespace, err := s.PendingNamespace(ctx)
	if err != nil {
		return err
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil {
		return err
	}
	creates, err := s.PendingCreates(ctx)
	if err != nil {
		return err
	}
	pubs, err := s.PendingPublications(ctx)
	if err != nil {
		return err
	}
	if len(jobs)+len(conflicts)+len(namespace)+len(actions)+len(creates)+len(pubs) > 0 {
		return ErrMountUnsynced
	}
	if err := s.Validate(ctx); err != nil {
		return err
	}
	status, err := s.HistoryStatus(ctx)
	if err != nil {
		return err
	}
	if status.Pending > 0 {
		if status.Pending > 50 {
			return ErrHistoryPending
		}
		r, err := m.historyRepository(ctx, o.Scope.ProfileID)
		if err != nil {
			return errors.Join(ErrHistoryPending, err)
		}
		if _, err := r.Ingest(ctx, s); err != nil {
			return errors.Join(ErrHistoryPending, err)
		}
		status, err = s.HistoryStatus(ctx)
		if err != nil {
			return err
		}
		if status.Pending != 0 {
			return ErrHistoryPending
		}
	}
	return nil
}

func (m *Manager) finishRemoval(ctx context.Context, id model.MountID) error {
	name := m.removalPath(id)
	if err := platform.CheckNoSymlinks(name); err != nil {
		return err
	}
	b, err := os.ReadFile(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var r removalRecord
	if len(b) > 64<<10 || json.Unmarshal(b, &r) != nil || r.Schema != 1 || r.Mount != id {
		return ErrConfig
	}
	return m.finishRemovalRecord(ctx, r)
}

func (m *Manager) finishRemovalRecord(ctx context.Context, r removalRecord) error {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return err
	}
	for _, mount := range c.Mounts {
		if mount.ID == r.Mount {
			return ErrPending
		}
	}
	if r.Schema != 1 || !validID(string(r.Mount)) || !filepath.IsAbs(r.Cache) || filepath.Base(r.Cache) != string(r.Mount) {
		return ErrConfig
	}
	for _, o := range r.Owners {
		if o.ConfigFile != m.options.ConfigFile || o.Scope.MountID != r.Mount {
			return ErrConfig
		}
		if err := validateInstallationOwner(o); err != nil {
			return err
		}
		if err := platform.CheckNoSymlinks(o.Root); err != nil {
			return err
		}
	}
	if err := platform.CheckNoSymlinks(r.Cache); err != nil {
		return err
	}
	// Keep the config discoverable after its last owner is removed. Lock files
	// retain their inode and are never unlinked while another process may use it.
	if err := atomicJSON(filepath.Join(m.ownerDir(), "config.ref"), configReference{Schema: 1, ConfigFile: m.options.ConfigFile}); err != nil {
		return err
	}
	// Remove ownership first; interrupted deletion must never leave an owner
	// pointing to a removed database and block a later installer audit.
	for _, o := range r.Owners {
		name := filepath.Join(m.ownerDir(), string(r.Mount)+"-"+hashPath(o.Root)+".json")
		if err := platform.CheckNoSymlinks(name); err != nil {
			return err
		}
		if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := platform.SyncDir(m.ownerDir()); err != nil {
		return err
	}
	for _, o := range r.Owners {
		if err := os.RemoveAll(o.Root); err != nil {
			return err
		}
		if err := platform.SyncDir(filepath.Dir(o.Root)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.RemoveAll(r.Cache); err != nil {
		return err
	}
	if err := platform.SyncDir(filepath.Dir(r.Cache)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// A mountpoint is a user-selected location, not an owned recursive cache.
	if filepath.IsAbs(r.Mountpoint) && platform.CheckNoSymlinks(r.Mountpoint) == nil {
		if info, err := os.Lstat(r.Mountpoint); err == nil && info.IsDir() {
			_ = os.Remove(r.Mountpoint)
		}
	}
	if err := os.Remove(m.removalPath(r.Mount)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return platform.SyncDir(filepath.Dir(m.removalPath(r.Mount)))
}

// Before a Store exists, failed startup may leave only these owned diagnostics.
// Unknown content, cache bytes and symlinks remain protected by default.
func auditUninitializedRemoval(o ownership) error {
	entries, err := os.ReadDir(o.Root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := filepath.Join(o.Root, entry.Name())
		if err := platform.CheckNoSymlinks(name); err != nil {
			return err
		}
		switch entry.Name() {
		case "last-state.json":
			if err := installationRegular(name); err != nil {
				return err
			}
			info, err := os.Stat(name)
			if err != nil {
				return err
			}
			if info.Size() > 64<<10 {
				return ErrConfig
			}
			b, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			var state map[string]string
			if json.Unmarshal(b, &state) != nil || len(state) != 1 {
				return ErrConfig
			}
			if _, ok := state["last_error"]; !ok {
				return ErrConfig
			}
		case "ipc.json":
			if _, _, err := readEndpointAt(o.Root, o.Scope.MountID, o.ConfigFile); err != nil {
				return err
			}
		case "ipc.key":
			if err := installationRegular(name); err != nil {
				return err
			}
			info, err := os.Stat(name)
			if err != nil {
				return err
			}
			if info.Size() != 64 {
				return ErrConfig
			}
			b, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := hex.DecodeString(string(b)); err != nil {
				return ErrConfig
			}
		case "transport":
			if err := installationUnusedRoot(name); err != nil {
				return err
			}
		case "logs":
			logs, err := os.ReadDir(name)
			if err != nil {
				return err
			}
			for _, log := range logs {
				file := filepath.Join(name, log.Name())
				if err := installationRegular(file); err != nil {
					return err
				}
				switch log.Name() {
				case "worker.stderr.log":
				case "events.jsonl":
					events, err := readLogs(file)
					if err != nil {
						return err
					}
					for _, event := range events {
						if event.MountID != o.Scope.MountID {
							return ErrConfig
						}
					}
				default:
					return ErrConfig
				}
			}
		default:
			return ErrConfig
		}
	}
	return nil
}

func (m *Manager) retryRemovals(ctx context.Context) {
	dir := filepath.Join(m.ownerDir(), "removals")
	if platform.CheckNoSymlinks(dir) != nil {
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		id := model.MountID(f.Name()[:len(f.Name())-len(filepath.Ext(f.Name()))])
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" || !validID(string(id)) {
			continue
		}
		// DeleteMount acquires the same domain/intent/run ownership as a user call.
		if _, err := m.ResolveMount(ctx, string(id)); err == nil {
			continue
		}
		_ = m.DeleteMount(ctx, id)
	}
}
