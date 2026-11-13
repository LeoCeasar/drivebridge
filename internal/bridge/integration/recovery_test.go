package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestConflictDraftCopiesAndManualResolveComposeAcrossModules(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	h.cloud.setDoc(h.docToken, `<p id="a">alpha REMOTE gamma</p><p id="anchor">stable anchor 中文😀</p><p id="b">tail</p>`)
	entry := h.save("document.md", "alpha LOCAL gamma\n\nstable anchor 中文😀\n\ntail\n")
	if err := h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	conflicts, err := h.store.Conflicts(h.ctx)
	if err != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, err)
	}
	c := conflicts[0]
	draft := []byte("alpha chosen gamma\n\nstable anchor 中文😀\n\ntail\n")
	if _, _, err = h.engine.Resolve(h.ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: "stale", Content: draft}); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	after, err := h.store.Conflicts(h.ctx)
	if err != nil || len(after) != 1 || h.blob(after[0].Draft) != string(draft) {
		t.Fatal(after, err)
	}
	copies, err := h.store.ExportConflictCopies(h.ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	copiedDraft, err := os.ReadFile(copies.Draft)
	if err != nil || !bytes.Equal(copiedDraft, draft) {
		t.Fatal(string(copiedDraft), err)
	}
	copiedRemote, err := os.ReadFile(copies.Remote)
	if err != nil || !strings.Contains(string(copiedRemote), "REMOTE") {
		t.Fatal(string(copiedRemote), err)
	}
	h.restart()
	if _, _, err = h.engine.Resolve(h.ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: draft}); err != nil {
		t.Fatal(err)
	}
	resolved := h.entry("document.md")
	if resolved.Generation != entry.Generation+1 || h.blob(resolved.Local) != string(draft) {
		t.Fatal(resolved)
	}
	if err = h.engine.SyncEntry(h.ctx, resolved.ID); err != nil {
		t.Fatal(err)
	}
	if h.read("document.md") != string(draft) || h.cloud.doc(t, h.docToken) != string(draft) {
		t.Fatal("manual resolution bytes changed")
	}
	if remaining, er := h.store.Conflicts(h.ctx); er != nil || len(remaining) != 0 {
		t.Fatal(remaining, er)
	}
}
func TestUpgradeBackupPreservesPendingUnknownAndRegeneratesConflictCopies(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	h.read("data.bin")
	pending := h.save("data.bin", "pending across upgrade")
	conflicted := h.save("document.md", "conflicting local\n")
	if err := h.store.SaveConflict(h.ctx, model.Conflict{EntryID: conflicted.ID, Generation: conflicted.Generation, Local: conflicted.Local, Remote: conflicted.Local, Reason: "integration upgrade fixture"}); err != nil {
		t.Fatal(err)
	}
	unknown := h.save("uncertain.bin", "unknown across upgrade")
	jobs, err := h.store.Pending(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var unknownJob model.Job
	for _, j := range jobs {
		if j.EntryID == unknown.ID {
			unknownJob = j
		}
	}
	if _, _, err = h.store.Prepare(h.ctx, unknownJob.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.store.MarkUnknown(h.ctx, unknownJob.ID); err != nil {
		t.Fatal(err)
	}
	h.assertIdle()
	originalConflicts, err := h.store.Conflicts(h.ctx)
	if err != nil || len(originalConflicts) != 1 {
		t.Fatal(originalConflicts, err)
	}
	originalCopies, err := h.store.ExportConflictCopies(h.ctx, originalConflicts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCopy, err := os.ReadFile(originalCopies.Local)
	if err != nil {
		t.Fatal(err)
	}
	m := h.migrator()
	backup := filepath.Join(h.root, "upgrade-backup")
	manifest, err := m.PrepareUpgrade(h.ctx, backup)
	if err != nil || manifest.Digest == "" {
		t.Fatal(manifest, err)
	}
	restore := filepath.Join(h.root, "restored-namespace")
	if _, err = m.Restore(h.ctx, backup, restore, map[model.MountID]store.Scope{h.scope.MountID: h.scope}); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Open(filepath.Join(restore, "mounts", string(h.scope.MountID)))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.BindScope(h.ctx, h.scope); err != nil {
		t.Fatal(err)
	}
	j, err := restored.Job(h.ctx, unknownJob.ID)
	if err != nil || j.State != model.StatusAwaitingVerification {
		t.Fatal(j, err)
	}
	entry, err := restored.Entry(h.ctx, pending.ID)
	if err != nil || entry.Local != pending.Local {
		t.Fatal(entry, err)
	}
	conflicts, err := restored.Conflicts(h.ctx)
	if err != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, err)
	}
	regenerated, err := restored.ExportConflictCopies(h.ctx, conflicts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	restoredCopy, err := os.ReadFile(regenerated.Local)
	if err != nil || !bytes.Equal(restoredCopy, beforeCopy) {
		t.Fatal(string(restoredCopy), err)
	}
	if err = restored.Validate(h.ctx); err != nil {
		t.Fatal(err)
	}
}
func TestDifferentAccountCannotRebindOldAcceptedQueue(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	entry := h.save("data.bin", "old identity accepted")
	h.vfs.Shutdown()
	h.vfs = nil
	if err := h.overlay.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.overlay = nil
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.store = nil
	opened, err := store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	other := h.scope
	other.ProfileID = model.ProfileID(uuid.NewString())
	other.IdentityHash = uuid.NewString()
	if err = opened.BindScope(context.Background(), other); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	if err = opened.BindScope(context.Background(), h.scope); err != nil {
		t.Fatal(err)
	}
	preserved, err := opened.Entry(context.Background(), entry.ID)
	if err != nil || preserved.Local != entry.Local {
		t.Fatal(preserved, err)
	}
	newDir := filepath.Join(h.root, "new-account-namespace")
	fresh, err := store.Open(newDir)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	other.MountID = model.MountID(uuid.NewString())
	other.RemoteID = model.RemoteID(uuid.NewString())
	if err = fresh.BindScope(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	jobs, err := fresh.Pending(context.Background())
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}
func TestLocalWholeReplacementUploadsWithoutManualConflict(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	want := "complete local replacement\n"
	entry := h.save("document.md", want)
	if err := h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	after := h.entry("document.md")
	if after.ID != entry.ID || after.Token != entry.Token || after.Status != model.StatusSynced || h.cloud.doc(t, entry.Token) != want {
		t.Fatal("clean replacement did not retain identity and content")
	}
	if conflicts, err := h.store.Conflicts(h.ctx); err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
}

func TestConflictingWholeReplacementKeepsConflictCopiesWithoutCloudOverwrite(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	// A local-only whole rewrite is now a clean Git merge. A simultaneous cloud
	// rewrite is the actual conflict case whose copies and recovery we protect.
	h.cloud.setDoc(h.docToken, `<p id="a">concurrent cloud replacement</p><p id="anchor">stable anchor 中文😀</p><p id="b">tail</p>`)
	original := h.cloud.doc(t, h.docToken)
	entry := h.save("document.md", "complete unanchored replacement\n")
	if err := h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	if h.cloud.doc(t, h.docToken) != original {
		t.Fatal("ambiguous replacement overwrote cloud")
	}
	conflicts, err := h.store.Conflicts(h.ctx)
	if err != nil || len(conflicts) != 1 || conflicts[0].Reason != "overlapping_edits" {
		t.Fatal(conflicts, err)
	}
	copies, err := h.store.ExportConflictCopies(h.ctx, conflicts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	local, err := os.ReadFile(copies.Local)
	if err != nil || string(local) != "complete unanchored replacement\n" {
		t.Fatal(string(local), err)
	}
	remote, err := os.ReadFile(copies.Remote)
	if err != nil || string(remote) != original {
		t.Fatal(string(remote), err)
	}
	if h.blob(entry.Local) != string(local) {
		t.Fatal("accepted replacement lost")
	}
	chosen := []byte("explicit human replacement 中文😀\n")
	c := conflicts[0]
	resolved, _, err := h.engine.Resolve(h.ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: chosen})
	if err != nil || resolved.ID != entry.ID || resolved.Token != entry.Token {
		t.Fatal(resolved, err)
	}
	// Human intent must survive process exit before any network mutation.
	h.restart()
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		h.debugState()
		t.Fatal(err)
	}
	after := h.entry("document.md")
	if after.ID != entry.ID || after.Token != entry.Token || after.Status != model.StatusSynced || h.read("document.md") != string(chosen) || h.cloud.doc(t, entry.Token) != string(chosen) {
		t.Fatal("explicit resolution lost resource identity or chosen bytes", after)
	}
	if remaining, err := h.store.Conflicts(h.ctx); err != nil || len(remaining) != 0 {
		t.Fatal(remaining, err)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.creates != 0 || h.cloud.deletes != 0 || len(h.cloud.applied) == 0 {
		t.Fatal("manual resolution recreated resource or omitted verified block operations")
	}
}

func TestNamespaceRestartReconcilesCommittedReplacementBeforeCacheReload(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "prepared"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.read("document.md")
			old := "old accepted target\n"
			target := h.save("document.md", old)
			replacement := "new accepted temporary 中文😀\n"
			source := h.save("editor.tmp", replacement)
			operation, err := h.overlay.BeginManagedRename(h.ctx, "editor.tmp", "document.md")
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				object, err := h.overlay.RenameManaged(h.ctx, operation)
				if err != nil || object == nil || object.Size() != int64(len(replacement)) {
					t.Fatal(object, err)
				}
			}
			journal, err := h.store.PendingNamespace(h.ctx)
			if err != nil || len(journal) != 1 || journal[0].State != name {
				t.Fatal(journal, err)
			}
			// Deliberately do not migrate VFS nodes/cache or FinishManagedRename:
			// this is the restart boundary between durable commit and cache repair.
			h.restart()
			after := h.entry("document.md")
			if after.ID != target.ID || after.Token != target.Token || after.FrozenBy != "" {
				t.Fatal(after)
			}
			if committed {
				if h.read("document.md") != replacement || h.blob(after.Local) != replacement {
					t.Fatal("stale target cache replaced committed input")
				}
				if _, err = h.store.EntryByRemote(h.ctx, "editor.tmp"); !errors.Is(err, store.ErrNotFound) {
					t.Fatal(err)
				}
				if _, err = h.vfs.Stat("editor.tmp"); err == nil {
					t.Fatal("committed source cache reappeared")
				}
			} else {
				if h.read("document.md") != old || h.read("editor.tmp") != replacement {
					t.Fatal("prepared rollback lost accepted source/target")
				}
				retained := h.entry("editor.tmp")
				if retained.ID != source.ID || retained.FrozenBy != "" {
					t.Fatal(retained)
				}
			}
			journal, err = h.store.PendingNamespace(h.ctx)
			if err != nil || len(journal) != 0 {
				t.Fatal(journal, err)
			}
			h.cloud.mu.Lock()
			mutations := h.cloud.creates + h.cloud.applies + h.cloud.uploads + h.cloud.deletes
			h.cloud.mu.Unlock()
			if mutations != 0 {
				t.Fatal("local replay dispatched cloud work")
			}
		})
	}
}
