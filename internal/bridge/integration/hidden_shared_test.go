package integration

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/overlay"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
	"github.com/rclone/rclone/vfs"
)

// Compose the production policy gates with real Store, Overlay, VFS and Engine.
// The provider is synthetic; no browser, account or system driver is required.
func TestHiddenSharedVFSRetainsAndResumesAcceptedUploads(t *testing.T) {
	h := newHarness(t)
	h.vfs.Shutdown()
	if err := h.overlay.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	var enabled atomic.Bool
	enabled.Store(true)
	const hidden = "hidden-root"
	const visible = "visible-root"
	const body = "hidden-file"
	const outside = "visible-file"
	snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: hidden, Name: "Hidden", Memberships: []string{discovery.SharedHidden}}, {Token: visible, Name: "Visible", Memberships: []string{discovery.SharedVisible}}}}
	h.cloud.mu.Lock()
	h.cloud.resources[hidden] = resource{kind: model.KindFolder, name: "Hidden", parent: "virtual-shared"}
	h.cloud.resources[visible] = resource{kind: model.KindFolder, name: "Visible", parent: "virtual-shared"}
	h.cloud.resources[body] = resource{kind: model.KindFile, name: "note.md", parent: hidden, data: []byte("hidden baseline\n")}
	h.cloud.resources[outside] = resource{kind: model.KindFile, name: "other.txt", parent: visible, data: []byte("visible baseline\n")}
	h.cloud.mu.Unlock()
	raw, err := overlay.New(overlay.Dependencies{Store: h.store, Source: overlaySource{h.cloud}, ProfileID: h.scope.ProfileID, MountID: h.scope.MountID, Account: &overlay.AccountOptions{AccountToken: h.scope.FolderToken, PersonalToken: "personal-root", ReadSnapshot: func(context.Context) (discovery.Snapshot, error) { return snapshot, nil }, SyncHiddenShared: func(context.Context) (bool, error) { return enabled.Load(), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	h.overlay = raw.(*overlay.Fs)
	options := h.vfs.Opt
	h.vfs, err = vfs.NewManaged(h.ctx, h.overlay, &options)
	if err != nil {
		t.Fatal(err)
	}
	h.engine, err = syncer.New(syncer.Dependencies{Store: h.store, Source: engineSource{h.cloud}, Publisher: syncer.PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		entry, err := h.store.Entry(ctx, c.EntryID)
		if err != nil {
			return err
		}
		return h.vfs.TryExternalUpdate(ctx, entry.Remote, func() (fs.Object, error) { return h.overlay.Publish(ctx, c) })
	}), ShouldSync: h.overlay.ShouldSync, ShouldSyncPath: h.overlay.SyncPathAllowed, ShouldRefresh: h.overlay.SyncEntryAllowed}, syncer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	hiddenPath := "共享目录/Hidden/note.md"
	visiblePath := "共享目录/Visible/other.txt"
	if h.read(hiddenPath) != "hidden baseline\n" || h.read(visiblePath) != "visible baseline\n" {
		t.Fatal("initial account view")
	}
	entry := h.save(hiddenPath, "accepted local markdown\n")
	enabled.Store(false)
	root, err := h.vfs.Root()
	if err != nil {
		t.Fatal(err)
	}
	root.ForgetAll()
	root.ForgetPath("共享目录/.policy-refresh", fs.EntryObject)
	if _, err = h.vfs.Stat("共享目录/Hidden"); err == nil {
		t.Fatal("disabled cached entrance still visible")
	}
	before, _ := h.store.Pending(h.ctx)
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.engine.RefreshEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := h.store.Pending(h.ctx)
	if len(before) != 1 || len(after) != 1 || h.blob(h.entry(hiddenPath).Local) != "accepted local markdown\n" {
		t.Fatal("pending hidden content lost", before, after)
	}
	h.cloud.mu.Lock()
	unchanged := string(h.cloud.resources[body].data) == "hidden baseline\n"
	uploads := h.cloud.uploads
	h.cloud.mu.Unlock()
	if !unchanged || uploads != 0 {
		t.Fatal("disabled content reached remote")
	}
	other := h.save(visiblePath, "visible changes still sync\n")
	if err = h.engine.SyncEntry(h.ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	enabled.Store(true)
	root.ForgetAll()
	root.ForgetPath("共享目录/.policy-refresh", fs.EntryObject)
	if _, err = h.vfs.Stat("共享目录/Hidden"); err != nil {
		t.Fatal("entrance not restored", err)
	}
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if h.read(hiddenPath) != "accepted local markdown\n" || h.entry(hiddenPath).ID != entry.ID || h.entry(hiddenPath).Token != body {
		t.Fatal("restored content/identity changed")
	}
	remaining, _ := h.store.Pending(h.ctx)
	if len(remaining) != 0 {
		t.Fatal(remaining)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.uploads != 2 || string(h.cloud.resources[body].data) != "accepted local markdown\n" || string(h.cloud.resources[outside].data) != "visible changes still sync\n" {
		t.Fatal("paused and visible jobs did not resume independently")
	}
}
