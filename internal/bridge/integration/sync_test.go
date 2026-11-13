package integration

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
	"github.com/rclone/rclone/vfs"
)

func TestInitialMetadataIsLazyAndTargetSizeMatchesImmutableBytes(t *testing.T) {
	h := newHarness(t)
	root, err := h.vfs.Root()
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := root.ReadDirAll()
	if err != nil || len(nodes) != 2 {
		t.Fatal(nodes, err)
	}
	h.cloud.mu.Lock()
	docReads, fileReads := h.cloud.fetches, h.cloud.observations
	h.cloud.mu.Unlock()
	if docReads+fileReads != 0 {
		t.Fatal("listing downloaded bodies")
	}
	n, err := h.vfs.Stat("document.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha beta gamma\n\nstable anchor 中文😀\n\ntail\n"
	if n.Size() != int64(len(want)) || h.read("document.md") != want {
		t.Fatal(n.Size())
	}
	file, err := h.vfs.Stat("data.bin")
	if err != nil || file.Size() != int64(len("\x00\xffinitial bytes")) {
		t.Fatal(file, err)
	}
	entry := h.entry("data.bin")
	sn, err := h.store.Snapshot(h.ctx, entry.RemoteCheckpoint)
	if err != nil || sn.FileData != entry.Visible || sn.FileIdentity == nil || sn.Revision != "" {
		t.Fatal(sn, err)
	}
}
func TestSavePartialMergeReadbackAndIdlePublication(t *testing.T) {
	h := newHarness(t)
	if h.read("document.md") != "alpha beta gamma\n\nstable anchor 中文😀\n\ntail\n" {
		t.Fatal("initial")
	}
	h.cloud.setDoc(h.docToken, `<p id="a">alpha beta GAMMA</p><p id="anchor">stable anchor 中文😀</p><p id="b">tail</p>`)
	h.cloud.mu.Lock()
	h.cloud.loseApplyResponse = true
	h.cloud.mu.Unlock()
	local := "ALPHA beta gamma\n\nstable anchor 中文😀\n\nTAIL local\n"
	entry := h.save("document.md", local)
	if entry.Generation != 1 || entry.Status != model.StatusQueued || h.blob(entry.Local) != local {
		t.Fatal(entry)
	}
	h.cloud.mu.Lock()
	writes := h.cloud.applies
	h.cloud.mu.Unlock()
	if writes != 0 {
		t.Fatal("local fsync contacted cloud")
	}
	if err := h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		h.debugState()
		t.Fatal(err)
	}
	h.assertIdle()
	want := "ALPHA beta GAMMA\n\nstable anchor 中文😀\n\nTAIL local\n"
	if h.read("document.md") != want || h.cloud.doc(t, h.docToken) != want {
		t.Fatal("merged bytes differ")
	}
	after := h.entry("document.md")
	if after.Status != model.StatusSynced || h.blob(after.LocalCheckpoint) != want {
		t.Fatal(after)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.applies != 2 || len(h.cloud.applied) != 2 {
		t.Fatal("partial operation replayed or lost", h.cloud.applies)
	}
	for _, op := range h.cloud.applied {
		if op.Kind != model.OperationBlockReplace || op.BlockID == "" {
			t.Fatal(op)
		}
	}
}
func TestOpenReaderDefersRemotePublicationUntilIdle(t *testing.T) {
	h := newHarness(t)
	if h.read("data.bin") != "\x00\xffinitial bytes" {
		t.Fatal("initial")
	}
	reader, err := h.vfs.Open("data.bin")
	if err != nil {
		t.Fatal(err)
	}
	h.cloud.setFile(h.fileToken, []byte("changed bytes after observation"))
	entry := h.entry("data.bin")
	if err = h.engine.RefreshEntry(h.ctx, entry.ID); !errors.Is(err, vfs.ErrExternalUpdateBusy) {
		t.Fatal(err)
	}
	old, err := io.ReadAll(reader)
	if err != nil || string(old) != "\x00\xffinitial bytes" {
		t.Fatal(string(old), err)
	}
	pending, err := h.store.PendingPublications(h.ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.read("data.bin"); got != "changed bytes after observation" {
		t.Fatal(got)
	}
}
func TestAtomicTemporarySaveKeepsDocumentIdentityAndNeverCreatesTemp(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	original := h.entry("document.md")
	h.save("editor.tmp", "replacement\n\nstable anchor 中文😀\n\ntail\n")
	if err := h.vfs.Rename("editor.tmp", "document.md"); err != nil {
		t.Fatal(err)
	}
	replacement := h.entry("document.md")
	if replacement.ID != original.ID || replacement.Token != original.Token || replacement.Kind != model.KindDocx || replacement.FrozenBy != "" {
		t.Fatal(replacement)
	}
	if _, err := h.store.EntryByRemote(h.ctx, "editor.tmp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if err := h.engine.SyncEntry(h.ctx, replacement.ID); err != nil {
		h.debugState()
		t.Fatal(err)
	}
	if h.read("document.md") != "replacement\n\nstable anchor 中文😀\n\ntail\n" || h.cloud.doc(t, h.docToken) != "replacement\n\nstable anchor 中文😀\n\ntail\n" {
		t.Fatal("replacement bytes")
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.creates != 0 || h.cloud.deletes != 0 {
		t.Fatal(h.cloud.creates, h.cloud.deletes)
	}
	for _, r := range h.cloud.resources {
		if strings.Contains(r.name, "editor") {
			t.Fatal("temporary uploaded")
		}
	}
}
func TestCapturedG1ConfirmationNeverClearsG2(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	first := h.save("data.bin", "G1 accepted")
	gate := &uploadGate{entered: make(chan struct{}), resume: make(chan struct{})}
	h.cloud.mu.Lock()
	h.cloud.gate = gate
	h.cloud.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- h.engine.SyncEntry(h.ctx, first.ID) }()
	await(t, gate.entered)
	second := h.save("data.bin", "G2 newest local")
	close(gate.resume)
	if err := await(t, done); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	after := h.entry("data.bin")
	if after.Generation != second.Generation || h.blob(after.Local) != "G2 newest local" || h.blob(after.LocalCheckpoint) != "G1 accepted" || after.Status == model.StatusSynced {
		t.Fatal(after)
	}
	if err := h.engine.SyncEntry(h.ctx, after.ID); err != nil {
		t.Fatal(err)
	}
	if h.read("data.bin") != "G2 newest local" {
		t.Fatal("G2 lost")
	}
}
func TestStoreFsyncFailureDoesNotRejectLocalSaveAndRetainsPendingBytes(t *testing.T) {
	for _, fault := range []struct {
		name string
		code int32
		want error
	}{{"EIO", 1, syscallEIO()}, {"ENOSPC", 2, syscallENOSPC()}} {
		t.Run(fault.name, func(t *testing.T) {
			h := newHarness(t)
			h.read("data.bin")
			before := h.entry("data.bin")
			f, err := h.vfs.OpenFile("data.bin", os.O_RDWR|os.O_TRUNC, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte("not yet accepted")); err != nil {
				t.Fatal(err)
			}
			h.io.errno.Store(fault.code)
			h.io.fail.Store(true)
			if err = f.Sync(); err != nil {
				t.Fatal(err)
			}
			if err = h.vfs.DrainManagedArchives(h.ctx); !errors.Is(err, fault.want) {
				t.Fatal("background acceptance did not report Store failure", err)
			}
			unchanged, err := h.store.EntryByRemote(h.ctx, "data.bin")
			if err != nil {
				t.Fatal(err)
			}
			if unchanged.Generation != before.Generation || unchanged.Local != before.Local {
				t.Fatal("failed sync changed accepted state")
			}
			dirty := make([]byte, len("not yet accepted"))
			if _, err = f.ReadAt(dirty, 0); err != nil || string(dirty) != "not yet accepted" {
				t.Fatal(string(dirty), err)
			}
			h.io.fail.Store(false)
			if err = f.Sync(); err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			accepted := h.entry("data.bin")
			if accepted.Generation != before.Generation+1 || h.blob(accepted.Local) != "not yet accepted" {
				t.Fatal(accepted)
			}
			h.restart()
			if h.read("data.bin") != "not yet accepted" {
				t.Fatal("accepted retry lost on restart")
			}

		})
	}
}
func TestOfflineAndPermissionRecoveryPreserveAcceptedNamespace(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	entry := h.save("data.bin", "offline saved")
	h.cloud.mu.Lock()
	h.cloud.offline = true
	h.cloud.mu.Unlock()
	if err := h.engine.SyncEntry(h.ctx, entry.ID); err == nil {
		t.Fatal("offline accepted as synced")
	}
	h.restart()
	after := h.entry("data.bin")
	if h.blob(after.Local) != "offline saved" {
		t.Fatal("offline lost")
	}
	h.cloud.mu.Lock()
	h.cloud.offline = false
	h.cloud.permission = true
	h.cloud.mu.Unlock()
	if err := h.engine.Retry(h.ctx, after.ID); err == nil {
		t.Fatal("permission denial hidden")
	}
	blocked := h.entry("data.bin")
	if blocked.Status != model.StatusBlocked || h.blob(blocked.Local) != "offline saved" {
		t.Fatal(blocked)
	}
	h.cloud.mu.Lock()
	h.cloud.permission = false
	h.cloud.mu.Unlock()
	if err := h.engine.Retry(h.ctx, after.ID); err != nil {
		t.Fatal(err)
	}
	if h.read("data.bin") != "offline saved" {
		t.Fatal("recovery changed input")
	}
}
func TestLostUploadResponseReopensUnknownAndVerifiesWithoutReupload(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	entry := h.save("data.bin", "server applied before lost reply")
	h.cloud.mu.Lock()
	h.cloud.uploadLoseAndOffline = true
	h.cloud.mu.Unlock()
	if err := h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	jobs, err := h.store.Pending(h.ctx)
	if err != nil || len(jobs) != 1 || jobs[0].State != model.StatusAwaitingVerification {
		t.Fatal(jobs, err)
	}
	if h.blob(jobs[0].Local) != "server applied before lost reply" {
		t.Fatal("uncertain input lost")
	}
	h.cloud.mu.Lock()
	h.cloud.offline = false
	h.cloud.mu.Unlock()
	h.restart()
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if h.read("data.bin") != "server applied before lost reply" {
		t.Fatal("confirmed bytes differ")
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.uploads != 1 {
		t.Fatal("unknown upload blindly repeated", h.cloud.uploads)
	}
}
func TestUnverifiedEmptyUploadRemainsDurableAndBlocked(t *testing.T) {
	h := newHarness(t)
	h.read("data.bin")
	entry := h.save("data.bin", "")
	engine, err := syncer.New(syncer.Dependencies{Store: h.store, Source: engineSource{h.cloud}, Publisher: syncer.PublisherFunc(func(context.Context, model.PublishCandidate) error {
		t.Fatal("unobserved empty was published")
		return nil
	})}, syncer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	blocked := h.entry("data.bin")
	if blocked.Status != model.StatusBlocked || h.blob(blocked.Local) != "" {
		t.Fatal(blocked)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.uploads != 0 {
		t.Fatal("unverified empty sent")
	}
}
func TestOversizeTemporaryReplacementPreservesBothAcceptedFiles(t *testing.T) {
	h := newHarness(t)
	original := h.read("document.md")
	target := h.entry("document.md")
	large := strings.Repeat("x", int(store.LargeSnapshotBytes)+1)
	temporary := h.save("oversize.tmp", large)
	if err := h.vfs.Rename("oversize.tmp", "document.md"); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	after := h.entry("document.md")
	source := h.entry("oversize.tmp")
	if after.ID != target.ID || after.Token != target.Token || after.Local != target.Local || h.read("document.md") != original {
		t.Fatal("rejected oversized rename changed target")
	}
	if source.ID != temporary.ID || source.FrozenBy != "" || h.blob(source.Local) != large {
		t.Fatal("accepted oversized temporary input was lost")
	}
}
func TestNewMarkdownCreationPersistsInputAndUniqueNameBeforeCloudCall(t *testing.T) {
	h := newHarness(t)
	content := "new document 中文😀\n"
	entry := h.save("new-note.md", content)
	h.cloud.mu.Lock()
	h.cloud.beforeCreate = func(ctx context.Context, parent, name string, _ []byte) error {
		creates, err := h.store.PendingCreates(ctx)
		if err != nil {
			return err
		}
		if len(creates) != 1 || creates[0].EntryID != entry.ID || creates[0].Input != entry.Local || name != creates[0].TemporaryName || name == creates[0].FinalName || parent != h.scope.FolderToken {
			return errors.New("creation dispatched without complete durable journal")
		}
		if h.blob(creates[0].Input) != content {
			return errors.New("create input not durable")
		}
		return nil
	}
	h.cloud.mu.Unlock()
	if err := h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	created := h.entry("new-note.md")
	if created.Kind != model.KindDocx || created.Token == "" || created.Status != model.StatusSynced || h.read("new-note.md") != content || h.cloud.doc(t, created.Token) != content {
		t.Fatal(created)
	}
	h.cloud.mu.Lock()
	resource := h.cloud.resources[created.Token]
	count := h.cloud.creates
	h.cloud.mu.Unlock()
	if resource.name != "new-note" || count != 1 {
		t.Fatal(resource.name, count)
	}
	pending, err := h.store.PendingCreates(h.ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
}
func TestUnknownOrdinaryCreationReconcilesUniqueTemporaryResourceAfterRestart(t *testing.T) {
	h := newHarness(t)
	body := "\x00new binary😀"
	entry := h.save("new-file.bin", body)
	h.cloud.mu.Lock()
	h.cloud.uploadLoseAndOffline = true
	h.cloud.mu.Unlock()
	if err := h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	creates, err := h.store.PendingCreates(h.ctx)
	if err != nil || len(creates) != 1 || creates[0].State != "unknown" {
		t.Fatal(creates, err)
	}
	h.cloud.mu.Lock()
	h.cloud.offline = false
	h.cloud.mu.Unlock()
	h.restart()
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	created := h.entry("new-file.bin")
	if created.Token == "" || created.Status != model.StatusSynced || h.read("new-file.bin") != body {
		t.Fatal(created)
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.creates != 1 || h.cloud.uploads != 1 || h.cloud.resources[created.Token].name != "new-file.bin" {
		t.Fatal("unknown create was repeated or left under temporary name", h.cloud.creates, h.cloud.uploads)
	}
}
