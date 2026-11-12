package syncer

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

type deletionSource struct {
	*fixtureCloud
	missing bool
	listErr error
	bodyErr error
}

func (s *deletionSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	if s.bodyErr != nil {
		return ObservedFile{}, s.bodyErr
	}
	if s.missing && token == "resource" {
		return ObservedFile{}, os.ErrNotExist
	}
	return s.fixtureCloud.ObserveFile(ctx, token)
}
func (s *deletionSource) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	items, err := s.fixtureCloud.List(ctx, parent)
	var out []larkcli.DriveItem
	for _, item := range items {
		if !s.missing || item.Token != "resource" {
			out = append(out, item)
		}
	}
	return out, err
}
func deletionFixture(t *testing.T) (*store.Store, *Engine, *deletionSource, model.Entry, model.Conflict) {
	t.Helper()
	s, engine, cloud, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "local edit\n")
	source := &deletionSource{fixtureCloud: cloud, missing: true}
	engine.source = source
	if err := engine.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	conflicts, err := s.Conflicts(bg)
	if err != nil || len(conflicts) != 1 || conflicts[0].Reason != model.ConflictRemoteDeleted {
		t.Fatal(conflicts, err)
	}
	if cloud.writes != 0 || cloud.deletes != 0 {
		t.Fatal("deletion detection mutated cloud")
	}
	return s, engine, source, entry, conflicts[0]
}
func TestRemoteDeletionRestoreLocalCreatesAndVerifiesNewResource(t *testing.T) {
	s, e, source, entry, c := deletionFixture(t)
	local, job, err := e.Resolve(bg, model.ResolveRequest{Action: model.ResolveRestoreLocal, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("chosen local version\n")})
	if err != nil || local.Token != "" || job.ID == "" {
		t.Fatal(local, job, err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	actual, err := s.Entry(bg, entry.ID)
	if err != nil || actual.Token != "created-file" || actual.Status != model.StatusSynced || string(source.file) != "chosen local version\n" || source.writes != 1 {
		t.Fatal(actual, source.writes, err)
	}
	conflicts, _ := s.Conflicts(bg)
	jobs, _ := s.Pending(bg)
	if len(conflicts)+len(jobs) != 0 {
		t.Fatal(conflicts, jobs)
	}
}
func TestRemoteDeletionAcceptSettlesWithoutUploading(t *testing.T) {
	s, e, source, entry, c := deletionFixture(t)
	_, _, err := e.Resolve(bg, model.ResolveRequest{Action: model.ResolveAcceptDelete, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EntryByRemote(bg, entry.Remote); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	conflicts, _ := s.Conflicts(bg)
	jobs, _ := s.Pending(bg)
	if len(conflicts)+len(jobs) != 0 || source.writes+source.deletes != 0 {
		t.Fatal(conflicts, jobs, source.writes)
	}
}

func TestRemoteDeletionRestoreKeepsOriginalCloudSpelling(t *testing.T) {
	s, e, source, entry, c := deletionFixture(t)
	var err error
	entry, err = s.ObserveEntry(bg, store.Observation{Remote: entry.Remote, Kind: entry.Kind, Token: entry.Token, CloudName: "report?.md"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = e.Resolve(bg, model.ResolveRequest{Action: model.ResolveRestoreLocal, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	for _, item := range source.items {
		if item.Token == "created-file" && item.Name == "report?.md" {
			return
		}
	}
	t.Fatal("restoration used the escaped/collision local spelling", source.items)
}
func TestRemoteDeletionRechecksReappearanceBeforeDecision(t *testing.T) {
	s, e, source, entry, c := deletionFixture(t)
	source.missing = false
	_, _, err := e.Resolve(bg, model.ResolveRequest{Action: model.ResolveAcceptDelete, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision})
	if !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	if _, err = s.EntryByRemote(bg, entry.Remote); err != nil {
		t.Fatal(err)
	}
	fresh, err := e.RefreshConflict(bg, c.ID)
	if err != nil || fresh.Reason != "remote_reappeared" || fresh.Remote == "" {
		t.Fatal(fresh, err)
	}
}
func TestRemoteDeletionListingFailurePreservesDecisionAndDraft(t *testing.T) {
	s, e, source, entry, c := deletionFixture(t)
	source.listErr = os.ErrPermission
	_, _, err := e.Resolve(bg, model.ResolveRequest{Action: model.ResolveRestoreLocal, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("saved draft\n")})
	if err == nil {
		t.Fatal("permission failure accepted as deleted")
	}
	conflicts, _ := s.Conflicts(bg)
	if len(conflicts) != 1 || blobText(t, s, conflicts[0].Draft) != "saved draft\n" || source.writes != 0 {
		t.Fatal(conflicts, source.writes)
	}
}
func TestRemoteDeletionDoesNotOverwriteReplacementAtSameName(t *testing.T) {
	_, e, source, entry, c := deletionFixture(t)
	source.items = append(source.items, larkcli.DriveItem{Token: "other-file", Name: entry.CloudName, Type: "file", ParentToken: "root"})
	_, _, err := e.Resolve(bg, model.ResolveRequest{Action: model.ResolveRestoreLocal, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision})
	if !errors.Is(err, model.ErrVersionChanged) || source.writes != 0 {
		t.Fatal(err, source.writes)
	}
}
func TestRemoteDeletionCleanRefreshRemovesPlaceholder(t *testing.T) {
	s, e, cloud, entry := setup(t, model.KindFile)
	e.source = &deletionSource{fixtureCloud: cloud, missing: true}
	_ = e.RefreshEntry(bg, entry.ID)
	if _, err := s.EntryByRemote(bg, entry.Remote); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	conflicts, _ := s.Conflicts(bg)
	if len(conflicts) != 0 {
		t.Fatal(conflicts)
	}
}
func TestRemoteDeletionReadErrorAloneCannotDeleteOrConflict(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "pending\n")
	e.source = &deletionSource{fixtureCloud: source, bodyErr: os.ErrPermission}
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("expected source failure")
	}
	conflicts, _ := s.Conflicts(bg)
	if len(conflicts) != 0 {
		t.Fatal("read failure became deletion", conflicts)
	}
	if _, err := s.EntryByRemote(bg, entry.Remote); err != nil {
		t.Fatal(err)
	}
}
func TestLocalDeletionSupersedesUploadAndDeletesCloud(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "pending local edit\n")
	op, err := s.PrepareNamespace(bg, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(bg, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.cycle(bg); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Pending(bg)
	actions, _ := s.NamespaceActions(bg)
	if source.deletes != 1 || source.writes != 0 || len(jobs)+len(actions) != 0 {
		t.Fatal(source.deletes, source.writes, jobs, actions)
	}
}

func TestLegacyUnsentDeletionDecisionNeedsNoCloudOrAuthentication(t *testing.T) {
	for _, action := range []string{model.ResolveAcceptDelete, model.ResolveRestoreLocal} {
		t.Run(action, func(t *testing.T) {
			s, e, source, _ := setup(t, model.KindFile)
			entry, err := s.EnsureEntry(bg, "unsent.md", model.KindFile)
			if err != nil {
				t.Fatal(err)
			}
			entry = stageContent(t, s, entry, "never uploaded\n")
			c := model.Conflict{ID: "legacy-delete", EntryID: entry.ID, Generation: entry.Generation, Local: entry.Local, Reason: "delete_vs_pending_edit"}
			if err = s.SaveConflict(bg, c); err != nil {
				t.Fatal(err)
			}
			checks := 0
			e.deps.CheckIdentity = func(context.Context) error { checks++; return os.ErrPermission }
			source.readErr = os.ErrPermission
			fresh, err := e.RefreshConflict(bg, c.ID)
			if err != nil || fresh.Reason != model.ConflictLocalDeletePending || checks != 0 {
				t.Fatal(fresh, checks, err)
			}
			request := model.ResolveRequest{Action: action, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: fresh.RemoteRevision}
			if action == model.ResolveRestoreLocal {
				request.Content = []byte("chosen local edit\n")
			}
			accepted, job, err := e.Resolve(bg, request)
			if err != nil || checks != 0 || source.writes+source.deletes+source.creates != 0 {
				t.Fatal(accepted, job, checks, err)
			}
			if action == model.ResolveRestoreLocal {
				if job.ID == "" || accepted.Token != "" || blobText(t, s, accepted.Local) != "chosen local edit\n" {
					t.Fatal(accepted, job)
				}
			} else if _, err = s.EntryByRemote(bg, entry.Remote); !errors.Is(err, store.ErrNotFound) {
				t.Fatal(err)
			}
		})
	}
}
