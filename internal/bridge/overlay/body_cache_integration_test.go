package overlay

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/rclone/rclone/backend/local"
	"github.com/rclone/rclone/fs"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
)

const integrationCacheName = "cache-lifecycle.txt"
const integrationCacheToken = "cache-lifecycle-token"

func bodyCacheIntegration(t *testing.T, rejectWrites *atomic.Bool) (*Fs, *store.Store, *fakeSource, *vfs.VFS, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenWithOptions(filepath.Join(root, "store"), store.Options{SpacePolicy: func(_ context.Context, _ string, _ int64) error {
		if rejectWrites != nil && rejectWrites.Load() {
			return store.ErrSpace
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	scope := store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "test-identity", FolderToken: "test-root"}
	if err := st.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("complete real cache body 中文\n", 512)
	source := &fakeSource{items: []larkcli.DriveItem{{Token: integrationCacheToken, Name: integrationCacheName, Type: "file"}}, bodies: map[string]string{integrationCacheToken: content}}
	raw, err := New(Dependencies{Store: st, Source: source, ProfileID: scope.ProfileID, MountID: scope.MountID})
	if err != nil {
		t.Fatal(err)
	}
	f := raw.(*Fs)
	t.Cleanup(func() { _ = f.Shutdown(ctx) })
	previous := fsconfig.GetCacheDir()
	if err := fsconfig.SetCacheDir(filepath.Join(root, "vfs-cache")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsconfig.SetCacheDir(previous) })
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.CachePollInterval = 0
	options.PollInterval = 0
	options.HandleCaching = 0
	managed, err := vfs.NewManaged(ctx, f, &options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(managed.Shutdown)
	if !managed.ManagedSyncEnabled() {
		t.Fatal("actual managed admission is not enabled")
	}
	return f, st, source, managed, content
}
func integrationBodyFetches(source *fakeSource) int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.observes
}
func integrationCandidate(t *testing.T, st *store.Store, id model.EntryID) store.CacheCandidate {
	t.Helper()
	candidates, err := st.CleanCacheCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.EntryID == id {
			return candidate
		}
	}
	t.Fatalf("clean body missing from real Store candidates: %+v", candidates)
	return store.CacheCandidate{}
}
func integrationAccess(t *testing.T, st *store.Store, id model.EntryID) time.Time {
	t.Helper()
	metadata, err := st.Metadata(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return metadata.LastBodyAccess
}
func integrationEvict(t *testing.T, f *Fs, st *store.Store, managed *vfs.VFS, candidate store.CacheCandidate) (called, evicted bool, err error) {
	t.Helper()
	err = managed.TryExternalUpdate(ctx, candidate.Remote, func() (fs.Object, error) {
		called = true
		var err error
		evicted, err = st.EvictCleanEntry(ctx, candidate.EntryID, candidate.Generation, candidate.AccessedAt)
		if err != nil {
			return nil, err
		}
		return f.NewObject(ctx, candidate.Remote)
	})
	return
}
func TestManagedBodyCacheRealLifecycleAndAccessCAS(t *testing.T) {
	f, st, source, managed, content := bodyCacheIntegration(t, nil)
	node, err := managed.StatMetadata(integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.EntryByRemote(ctx, integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	if !integrationAccess(t, st, entry.ID).IsZero() {
		t.Fatal("metadata enumeration counted as body access")
	}
	for i := 0; i < 3; i++ {
		node.Size()
		node.ModTime()
		node.DirEntry().Size()
		node.DirEntry().ModTime(ctx)
	}
	if !integrationAccess(t, st, entry.ID).IsZero() || integrationBodyFetches(source) != 0 || node.DirEntry().Size() != -1 {
		t.Fatal("StatMetadata/Size/ModTime materialized or touched a placeholder")
	}

	first, err := node.Open(os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Release() })
	data, err := io.ReadAll(first)
	if err != nil || string(data) != content {
		t.Fatal("initial real VFS read", len(data), err)
	}
	firstEntry, err := st.Entry(ctx, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	access := integrationAccess(t, st, entry.ID)
	if access.IsZero() || integrationBodyFetches(source) != 1 {
		t.Fatal(access, integrationBodyFetches(source))
	}
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}
	candidate := store.CacheCandidate{EntryID: entry.ID, Remote: entry.Remote, Generation: firstEntry.Generation, AccessedAt: access}
	if called, evicted, err := integrationEvict(t, f, st, managed, candidate); !errors.Is(err, vfs.ErrExternalUpdateBusy) || called || evicted {
		t.Fatal("unclosed read admission allowed eviction", called, evicted, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if got := managed.ManagedCacheBytes(); got != int64(len(content)) {
		t.Fatalf("real VFS cache not fully populated: %d", got)
	}
	oldCandidate := integrationCandidate(t, st, entry.ID)

	// No invalidation occurred: the complete real VFS cache satisfies this open.
	// The remote seam is disabled so any second materialization would fail.
	source.mu.Lock()
	source.bodyErr = errors.New("unexpected remote fetch on cache hit")
	source.mu.Unlock()
	second, err := managed.Open(integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Release() })
	data, err = io.ReadAll(second)
	if err != nil || string(data) != content {
		t.Fatal("VFS cache hit lost complete bytes", len(data), err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	newAccess := integrationAccess(t, st, entry.ID)
	if !newAccess.After(oldCandidate.AccessedAt) || integrationBodyFetches(source) != 1 {
		t.Fatal("cache hit bypassed ManagedAccessTracker", oldCandidate.AccessedAt, newAccess, integrationBodyFetches(source))
	}
	node, err = managed.StatMetadata(integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	node.Size()
	node.ModTime()
	node.DirEntry().Size()
	node.DirEntry().ModTime(ctx)
	if !integrationAccess(t, st, entry.ID).Equal(newAccess) {
		t.Fatal("warm metadata changed LRU access time")
	}
	if called, evicted, err := integrationEvict(t, f, st, managed, oldCandidate); err != nil || !called || evicted {
		t.Fatal("candidate chosen before a newer Open evicted content", called, evicted, err)
	}
	stillCached, err := st.Entry(ctx, entry.ID)
	if err != nil || stillCached.Visible == "" || stillCached.Local != firstEntry.Local {
		t.Fatal(stillCached, err)
	}

	// Age only this test's body beyond preparation grace to verify actual removal.
	lease, err := st.ReadBlob(ctx, stillCached.Local)
	if err != nil {
		t.Fatal(err)
	}
	bodyPath := lease.(interface{ Name() string }).Name()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-store.PreparedBlobGrace - time.Hour)
	if err := os.Chtimes(bodyPath, aged, aged); err != nil {
		t.Fatal(err)
	}
	archiveStoreHistory(t, st)
	freshCandidate := integrationCandidate(t, st, entry.ID)
	if called, evicted, err := integrationEvict(t, f, st, managed, freshCandidate); err != nil || !called || !evicted {
		t.Fatal("closed clean body did not evict", called, evicted, err)
	}
	placeholder, err := st.EntryByRemote(ctx, integrationCacheName)
	if err != nil || placeholder.ID != entry.ID || placeholder.Token != entry.Token || placeholder.Remote != entry.Remote || placeholder.Visible != "" || placeholder.Local != "" {
		t.Fatal("eviction lost placeholder identity", placeholder, err)
	}
	if _, err := os.Stat(bodyPath); !os.IsNotExist(err) {
		t.Fatal("unreferenced clean body retained", err)
	}
	node, err = managed.StatMetadata(integrationCacheName)
	if err != nil || node.DirEntry().Size() != -1 || node.Size() != 0 || managed.ManagedCacheBytes() != 0 || integrationBodyFetches(source) != 1 {
		t.Fatal("eviction did not publish an unmaterialized placeholder", node, err, managed.ManagedCacheBytes(), integrationBodyFetches(source))
	}
	if !integrationAccess(t, st, entry.ID).Equal(newAccess) {
		t.Fatal("placeholder stat extended access time")
	}

	updated := strings.Repeat("rehydrated nonempty bytes 完整\n", 731)
	source.mu.Lock()
	source.bodyErr = nil
	source.bodies[integrationCacheToken] = updated
	source.mu.Unlock()
	reopened, err := node.Open(os.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Release() })
	data, err = io.ReadAll(reopened)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if string(data) != updated || len(data) == 0 || integrationBodyFetches(source) != 2 {
		t.Fatal("placeholder re-open returned stale/empty bytes", len(data), integrationBodyFetches(source))
	}
	rehydrated, err := st.Entry(ctx, entry.ID)
	if err != nil || rehydrated.ID != entry.ID || rehydrated.Token != entry.Token || rehydrated.Remote != entry.Remote || rehydrated.Visible == "" {
		t.Fatal(rehydrated, err)
	}
	if err := st.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestManagedDirtyCacheRejectsEvictionAfterFailedDurableAcceptance(t *testing.T) {
	var rejectWrites atomic.Bool
	f, st, source, managed, content := bodyCacheIntegration(t, &rejectWrites)
	reader, err := managed.Open(integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Release() })
	if data, err := io.ReadAll(reader); err != nil || string(data) != content {
		t.Fatal(len(data), err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := st.EntryByRemote(ctx, integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	candidate := integrationCandidate(t, st, before.ID)
	writer, err := managed.OpenFile(integrationCacheName, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Release() })
	if n, err := writer.WriteAt([]byte("DIRTY"), 0); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	rejectWrites.Store(true)
	if err := writer.Close(); err != nil {
		t.Fatal("a background Store failure must not reject a durable local save", err)
	}
	if err := managed.DrainManagedArchives(ctx); !errors.Is(err, store.ErrSpace) {
		t.Fatal("background Store acceptance did not report failure", err)
	}
	reads, writes, namespaces := managed.ManagedActivity()
	if reads+writes+namespaces != 0 {
		t.Fatal("closed handle still holds admission", reads, writes, namespaces)
	}
	if called, evicted, err := integrationEvict(t, f, st, managed, candidate); !errors.Is(err, vfs.ErrExternalUpdateBusy) || called || evicted {
		t.Fatal("closed dirty VFS cache allowed Store eviction", called, evicted, err)
	}
	stillClean, err := st.Entry(ctx, before.ID)
	if err != nil || stillClean.Visible != before.Visible || stillClean.Generation != before.Generation {
		t.Fatal("failed acceptance changed accepted Store body", stillClean, err)
	}
	if jobs, err := st.Pending(ctx); err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}

	// Recover through the same real VFS cache; successful Close durably accepts it.
	rejectWrites.Store(false)
	recovery, err := managed.Open(integrationCacheName)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovery.Release() })
	data, err := io.ReadAll(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "DIRTY"+content[5:] {
		t.Fatal("dirty cached bytes were lost", len(data))
	}
	if err := recovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := managed.DrainManagedArchives(ctx); err != nil {
		t.Fatal(err)
	}
	if jobs, err := st.Pending(ctx); err != nil || len(jobs) != 1 || jobs[0].EntryID != before.ID {
		t.Fatal(jobs, err)
	}
	if integrationBodyFetches(source) != 1 {
		t.Fatal("dirty cache recovery fetched cloud bytes", integrationBodyFetches(source))
	}
	if err := st.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}
