package integration

import (
	"errors"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/vfs"
)

func TestDeletionActualVFSLocalUnlinkSupersedesUpload(t *testing.T) {
	h := newHarness(t)
	h.save("data.bin", "local pending edit")
	if err := h.vfs.Remove("data.bin"); err != nil {
		t.Fatal(err)
	}
	// Exercise deletion explicitly; automatic Run now waits for its fixed
	// five-minute boundary rather than dispatching immediately on startup.
	if err := h.engine.SyncNow(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.cloud.mu.Lock()
	deletes, uploads := h.cloud.deletes, h.cloud.uploads
	_, remains := h.cloud.resources[h.fileToken]
	h.cloud.mu.Unlock()
	jobs, _ := h.store.Pending(h.ctx)
	actions, _ := h.store.NamespaceActions(h.ctx)
	if deletes != 1 || uploads != 0 || remains || len(jobs)+len(actions) != 0 {
		t.Fatal(deletes, uploads, remains, jobs, actions)
	}
}

func TestDeletionActualVFSRemoteCleanWaitsForReader(t *testing.T) {
	h := newHarness(t)
	_ = h.read("data.bin")
	handle, err := h.vfs.Open("data.bin")
	if err != nil {
		t.Fatal(err)
	}
	h.cloud.mu.Lock()
	returned := h.cloud.resources[h.fileToken]
	delete(h.cloud.resources, h.fileToken)
	h.cloud.mu.Unlock()
	if err = h.overlay.RefreshDirectory(h.ctx, ""); !errors.Is(err, vfs.ErrExternalUpdateBusy) {
		t.Fatal("open reader did not block deletion", err)
	}
	if _, err = h.store.EntryByRemote(h.ctx, "data.bin"); err != nil {
		t.Fatal(err)
	}
	if err = handle.Release(); err != nil {
		t.Fatal(err)
	}
	if err = h.overlay.RefreshDirectory(h.ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = h.store.EntryByRemote(h.ctx, "data.bin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = h.vfs.Stat("data.bin"); err == nil {
		t.Fatal("removed inode remained visible")
	}
	returned.data = []byte("restored cloud file with a different length")
	h.cloud.mu.Lock()
	h.cloud.resources[h.fileToken] = returned
	h.cloud.mu.Unlock()
	if err = h.overlay.RefreshDirectory(h.ctx, ""); err != nil {
		t.Fatal(err)
	}
	// The production metadata indexer emits this after its local refresh.
	// A bare Overlay refresh deliberately owns no VFS directory cache.
	h.vfs.NotifyManagedDirectoryChanged("")
	if got := h.read("data.bin"); got != string(returned.data) {
		t.Fatal("reappeared file reused old body", got)
	}
}

func TestDeletionActualVFSBinaryRestoreUploadsLocalBytes(t *testing.T) {
	h := newHarness(t)
	want := "\x00\xffnew local body\x00"
	entry := h.save("data.bin", want)
	h.cloud.mu.Lock()
	delete(h.cloud.resources, h.fileToken)
	h.cloud.mu.Unlock()
	if err := h.overlay.RefreshDirectory(h.ctx, ""); err != nil {
		t.Fatal(err)
	}
	conflicts, err := h.store.Conflicts(h.ctx)
	if err != nil || len(conflicts) != 1 || conflicts[0].Reason != model.ConflictRemoteDeleted {
		t.Fatal(conflicts, err)
	}
	c, err := h.engine.RefreshConflict(h.ctx, conflicts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = h.engine.Resolve(h.ctx, model.ResolveRequest{Action: model.ResolveRestoreLocal, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	restored := h.entry("data.bin")
	if restored.Token == h.fileToken || restored.Token == "" || restored.Status != model.StatusSynced || h.read("data.bin") != want {
		t.Fatal(restored)
	}
	h.cloud.mu.Lock()
	data, uploads := h.cloud.resources[restored.Token], h.cloud.uploads
	h.cloud.mu.Unlock()
	if string(data.data) != want || uploads != 1 {
		t.Fatal(data, uploads)
	}
}
