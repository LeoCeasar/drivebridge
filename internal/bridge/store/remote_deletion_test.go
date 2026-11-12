package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func deletionCleanEntry(t *testing.T, s *Store) model.Entry {
	t.Helper()
	e := bind(t, s, entry(t, s, "deleted.bin"), "gone-token")
	sn := fileSnapshot(t, s, e.Token, "accepted cloud content")
	e, err := s.AdoptFileSnapshot(ctx, e.ID, e.Generation, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRemoteDeletionCleanAndStaleCAS(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	stale := e
	stale.Generation++
	if _, _, err := s.ObserveRemoteDeletion(ctx, stale); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	c, removed, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || !removed || c.ID != "" {
		t.Fatal(c, removed, err)
	}
	got, err := s.Entry(ctx, e.ID)
	if err != nil || got.Generation != e.Generation+1 || got.Status != model.StatusSynced || got.Token != e.Token {
		t.Fatal(got, err)
	}
	if _, err = s.EntryByRemote(ctx, e.Remote); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err = s.Stage(ctx, e.ID, &e.Generation, strings.NewReader("late edit")); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	if _, removed, err = s.ObserveRemoteDeletion(ctx, got); err != nil || removed {
		t.Fatal(removed, err)
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != 0 {
		t.Fatal(actions, err)
	}
	if body := read(t, s, got.Local); body != "accepted cloud content" {
		t.Fatal(body)
	}
}

func TestRemoteDeletionConflictStableAndRestore(t *testing.T) {
	s, dir, scope := openTest(t)
	e := deletionCleanEntry(t, s)
	e, old := stage(t, s, e, "local change")
	// A worker may have captured the job, provided no external plan was recorded.
	if _, _, err := s.Prepare(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	c, removed, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || removed || c.Reason != model.ConflictRemoteDeleted || c.RemoteRevision != model.DeletedRevision(e.Token) || c.Local != e.Local || c.Remote != "" {
		t.Fatal(c, removed, err)
	}
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("draft recovery")); err != nil {
		t.Fatal(err)
	}
	again, removed, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || removed || again.ID != c.ID || read(t, s, again.Draft) != "draft recovery" {
		t.Fatal(again, removed, err)
	}
	r := model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: model.ResolveRestoreLocal}
	bad := r
	bad.ExpectedRemoteRevision = "stale"
	if _, _, err = s.ResolveRemoteDeletion(ctx, bad); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	restored, job, err := s.ResolveRemoteDeletion(ctx, r)
	if err != nil || restored.Token != "" || restored.LocalCheckpoint != "" || restored.RemoteCheckpoint != "" || restored.Remote != e.Remote || restored.ID != e.ID || restored.Generation != e.Generation+1 || restored.Local != e.Local || job.BaseLocal != "" || job.BaseRemote != "" {
		t.Fatal(restored, job, err)
	}
	if _, err = s.EntryByToken(ctx, e.Token); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	jobs, err := s.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatal(jobs, err)
	}
	if _, _, err = s.ResolveRemoteDeletion(ctx, r); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID {
		t.Fatal(jobs, err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareCreate(ctx, job.ID, "parent", "deleted.bin"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteDeletionAcceptSettlesAllConflictsAndQueues(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, _ = stage(t, s, e, "first local")
	old := model.Conflict{ID: "older", EntryID: e.ID, Generation: e.Generation, Base: e.LocalCheckpoint, Local: e.Local, Remote: e.LocalCheckpoint, RemoteRevision: "old", Reason: "concurrent_edit"}
	if err := s.SaveConflict(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDraft(ctx, old.ID, strings.NewReader("saved draft")); err != nil {
		t.Fatal(err)
	}
	e, _ = stage(t, s, e, "latest local")
	duplicate := old
	duplicate.ID = "another"
	duplicate.Generation = e.Generation
	duplicate.Local = e.Local
	if err := s.SaveConflict(ctx, duplicate); err != nil {
		t.Fatal(err)
	}
	c, removed, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || removed || c.ID == "" || c.Remote != "" {
		t.Fatal(c, removed, err)
	}
	current, err := s.Entry(ctx, e.ID)
	if err != nil || current.Local != e.Local {
		t.Fatal(current, err)
	}
	request := model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: model.ResolveAcceptDelete}
	// A later accepted local edit invalidates a human decision on earlier bytes.
	edited, _ := stage(t, s, current, "even newer local")
	if _, _, err = s.ResolveRemoteDeletion(ctx, request); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	c, _, err = s.ObserveRemoteDeletion(ctx, edited)
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedGeneration = edited.Generation
	result, job, err := s.ResolveRemoteDeletion(ctx, request)
	if err != nil || job.ID != "" || result.Generation != edited.Generation+1 || result.Token != e.Token {
		t.Fatal(result, job, err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
	if _, err = s.EntryByRemote(ctx, e.Remote); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if read(t, s, result.Local) != "even newer local" {
		t.Fatal("recovery lost local body")
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != 0 {
		t.Fatal(actions, err)
	}
}

func TestRemoteDeletionRestoreExplicitEmptyContent(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, _ = stage(t, s, e, "local")
	c, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: model.ResolveRestoreLocal, Content: []byte{}})
	if err != nil || read(t, s, got.Local) != "" {
		t.Fatal(got, err)
	}
}

func TestRemoteDeletionRejectsUnsafeWork(t *testing.T) {
	for _, state := range []string{"unknown", "plan", "create", "publication", "namespace"} {
		t.Run(state, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := deletionCleanEntry(t, s)
			e, job := stage(t, s, e, "local")
			if _, _, err := s.Prepare(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			want := ErrBusy
			switch state {
			case "unknown":
				if err := s.MarkUnknown(ctx, job.ID); err != nil {
					t.Fatal(err)
				}
				want = model.ErrUnknownResult
			case "plan":
				if err := s.RecordPlan(ctx, job.ID, []model.Operation{{ID: "op"}}, job.Local); err != nil {
					t.Fatal(err)
				}
			case "create":
				// A journal for the same entry can remain ambiguous after identity binding.
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					return put(tx, "createops", "uncertain", model.CreateOp{ID: "uncertain", EntryID: e.ID, JobID: job.ID, State: "unknown"})
				}); err != nil {
					t.Fatal(err)
				}
				want = model.ErrUnknownResult
			case "publication":
				sn := fileSnapshot(t, s, e.Token, "confirmed")
				if _, err := s.Confirm(ctx, job.ID, sn); err != nil {
					t.Fatal(err)
				}
				var err error
				e, err = s.Entry(ctx, e.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "namespace":
				// End preparation before reserving the namespace.
				if err := s.SaveConflict(ctx, model.Conflict{ID: "editing", EntryID: e.ID, Generation: e.Generation, Local: e.Local}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "new.bin"}); err != nil {
					t.Fatal(err)
				}
				want = ErrFrozen
			}
			if _, _, err := s.ObserveRemoteDeletion(ctx, e); !errors.Is(err, want) {
				t.Fatalf("wanted %v, got %v", want, err)
			}
			if _, err := s.EntryByRemote(ctx, e.Remote); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoteDeletionResolveRejectsNewUnknownJournal(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, job := stage(t, s, e, "local")
	c, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.update(ctx, func(tx *bolt.Tx) error {
		rec, err := jobTx(tx, job.ID)
		if err != nil {
			return err
		}
		rec.Job.State = model.StatusAwaitingVerification
		return saveJob(tx, rec)
	}); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{model.ResolveRestoreLocal, model.ResolveAcceptDelete} {
		if _, _, err = s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: action}); !errors.Is(err, model.ErrUnknownResult) {
			t.Fatal(action, err)
		}
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, err)
	}
}

func deletionNamespace(t *testing.T, s *Store, e model.Entry) model.NamespaceOp {
	t.Helper()
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: e.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestLocalDeleteResolvesOldConflicts(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, _ = stage(t, s, e, "local")
	c, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("recovery draft")); err != nil {
		t.Fatal(err)
	}
	op := deletionNamespace(t, s, e)
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != 1 || actions[0].ID != op.ID {
		t.Fatal(actions, err)
	}
}

func TestAcknowledgeMissingDeleteNeverClearsDispatchedUnknown(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "undispatched", true: "unknown"}[dispatched], func(t *testing.T) {
			s, _, _ := openTest(t)
			op := deletionNamespace(t, s, deletionCleanEntry(t, s))
			if dispatched {
				if err := s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
					t.Fatal(err)
				}
			}
			err := s.AcknowledgeMissingDelete(ctx, op.ID)
			if dispatched && !errors.Is(err, model.ErrUnknownResult) || !dispatched && err != nil {
				t.Fatal(err)
			}
			actions, err := s.NamespaceActions(ctx)
			want := 0
			if dispatched {
				want = 1
			}
			if err != nil || len(actions) != want {
				t.Fatal(actions, err)
			}
			if !dispatched {
				if err = s.AcknowledgeMissingDelete(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRemoteDeletionConflictRefreshRetainsRecoveryInputs(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, _ = stage(t, s, e, "local edit")
	old := model.Conflict{ID: "existing", EntryID: e.ID, Generation: e.Generation, Base: e.LocalCheckpoint, Local: e.Local, Remote: e.LocalCheckpoint, RemoteRevision: "prior-remote", Reason: "concurrent_edit"}
	if err := s.SaveConflict(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDraft(ctx, old.ID, strings.NewReader("my draft")); err != nil {
		t.Fatal(err)
	}
	c, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || c.ID != old.ID || c.Base != old.Base || c.Local != old.Local || read(t, s, c.Draft) != "my draft" || c.Remote != "" {
		t.Fatal(c, err)
	}
	if err = s.SaveConflict(ctx, old); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	if _, _, err = s.Resolve(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("ordinary resolve bypass")}); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestRemoteDeletionCASChecksIdentityAndCheckpoints(t *testing.T) {
	for _, field := range []string{"token", "path", "local", "local_checkpoint", "remote_checkpoint"} {
		t.Run(field, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := deletionCleanEntry(t, s)
			expected := e
			want := model.ErrVersionChanged
			switch field {
			case "token":
				expected.Token = "other"
				want = model.ErrIdentityChanged
			case "path":
				expected.Remote = "other"
				want = model.ErrIdentityChanged
			case "local":
				expected.Local = "stale"
			case "local_checkpoint":
				expected.LocalCheckpoint = "stale"
			case "remote_checkpoint":
				expected.RemoteCheckpoint = "stale"
			}
			if _, _, err := s.ObserveRemoteDeletion(ctx, expected); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if got, err := s.EntryByRemote(ctx, e.Remote); err != nil || got != e {
				t.Fatal(got, err)
			}
		})
	}
}

func TestRemoteDeletionSkipsUnboundAndFolders(t *testing.T) {
	s, _, _ := openTest(t)
	unbound := entry(t, s, "local.bin")
	folder, err := s.ObserveEntry(ctx, Observation{Remote: "folder", Kind: model.KindFolder, Token: "folder-token"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []model.Entry{unbound, folder} {
		c, removed, err := s.ObserveRemoteDeletion(ctx, e)
		if err != nil || removed || c.ID != "" {
			t.Fatal(c, removed, err)
		}
		if got, err := s.EntryByRemote(ctx, e.Remote); err != nil || got != e {
			t.Fatal(got, err)
		}
	}
}

func TestLocalDeleteRejectsLateConflictCreation(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, _ = stage(t, s, e, "local")
	deletionNamespace(t, s, e)
	late := model.Conflict{ID: "stale-worker", EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "concurrent_edit"}
	if err := s.SaveConflict(ctx, late); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
}

func TestRemoteDeletionDocxRestoresNewCreation(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.ObserveEntry(ctx, Observation{Remote: "document.md", Kind: model.KindDocx, Token: "doc-token"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ = stage(t, s, e, "# kept local document")
	c, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	restored, job, err := s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: model.ResolveRestoreLocal, Content: []byte("# chosen resolution")})
	if err != nil || restored.Kind != model.KindDocx || restored.Token != "" || read(t, s, restored.Local) != "# chosen resolution" {
		t.Fatal(restored, job, err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	create, err := s.PrepareCreate(ctx, job.ID, "parent", "document")
	if err != nil || create.Kind != model.KindDocx {
		t.Fatal(create, err)
	}
}

func TestRefreshReappearedConflictKeepsDraftAndClearsDeletionChoice(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, job := stage(t, s, e, "latest local")
	c, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("newer draft during network read")); err != nil {
		t.Fatal(err)
	}
	remote := fileSnapshot(t, s, e.Token, "reappeared cloud")
	if err = s.RefreshReappearedConflict(ctx, c, remote); err != nil {
		t.Fatal(err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, err)
	}
	got := conflicts[0]
	if got.ID != c.ID || got.Generation != c.Generation || got.Local != c.Local || got.Base != c.Base || got.Reason != "remote_reappeared" || got.Remote != remote.ContentBlob() || got.RemoteRevision != remote.ConflictVersion() || read(t, s, got.Draft) != "newer draft during network read" {
		t.Fatal(got)
	}
	current, err := s.Entry(ctx, e.ID)
	if err != nil || current.Generation != e.Generation || current.Token != e.Token || current.Local != e.Local {
		t.Fatal(current, err)
	}
	jobs, err := s.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].State != model.StatusConflict {
		t.Fatal(jobs, err)
	}
	for _, action := range []string{model.ResolveRestoreLocal, model.ResolveAcceptDelete} {
		if _, _, err = s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: action}); !errors.Is(err, ErrInvalidState) {
			t.Fatal(action, err)
		}
	}
	if err = s.RefreshReappearedConflict(ctx, c, remote); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
}

func TestRefreshReappearedConflictRejectsStaleCapture(t *testing.T) {
	for _, change := range []string{"local_edit", "conflict_input", "snapshot", "token"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := deletionCleanEntry(t, s)
			e, _ = stage(t, s, e, "local")
			c, _, err := s.ObserveRemoteDeletion(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			remote := fileSnapshot(t, s, e.Token, "cloud")
			want := model.ErrVersionChanged
			switch change {
			case "local_edit":
				if _, _, err = s.Stage(ctx, e.ID, &e.Generation, strings.NewReader("concurrent save")); err != nil {
					t.Fatal(err)
				}
				want = model.ErrGenerationChanged
			case "conflict_input":
				updated := c
				updated.Local = e.LocalCheckpoint
				if err = s.SaveConflict(ctx, updated); err != nil {
					t.Fatal(err)
				}
			case "snapshot":
				remote.FileData = e.Local
			case "token":
				remote = fileSnapshot(t, s, "different-token", "cloud")
				want = model.ErrIdentityChanged
			}
			if err = s.RefreshReappearedConflict(ctx, c, remote); !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
			conflicts, err := s.Conflicts(ctx)
			if err != nil || len(conflicts) != 1 || conflicts[0].Reason != model.ConflictRemoteDeleted {
				t.Fatal(conflicts, err)
			}
		})
	}
}

func TestRemoteDeletionRefreshUpdatesEveryUnresolvedConflictID(t *testing.T) {
	s, _, _ := openTest(t)
	e := deletionCleanEntry(t, s)
	e, _ = stage(t, s, e, "older local")
	old := model.Conflict{ID: "first", EntryID: e.ID, Generation: e.Generation, Base: e.LocalCheckpoint, Local: e.Local, Remote: e.LocalCheckpoint, RemoteRevision: "older", Reason: "concurrent_edit"}
	if err := s.SaveConflict(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDraft(ctx, old.ID, strings.NewReader("first draft")); err != nil {
		t.Fatal(err)
	}
	e, _ = stage(t, s, e, "latest local")
	other := old
	other.ID = "requested"
	other.Generation = e.Generation
	other.Local = e.Local
	if err := s.SaveConflict(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDraft(ctx, other.ID, strings.NewReader("requested draft")); err != nil {
		t.Fatal(err)
	}
	chosen, removed, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || removed || chosen.ID != old.ID {
		t.Fatal(chosen, removed, err)
	}
	all, err := s.Conflicts(ctx)
	if err != nil || len(all) != 2 {
		t.Fatal(all, err)
	}
	for _, c := range all {
		wantDraft := "first draft"
		if c.ID == other.ID {
			wantDraft = "requested draft"
		}
		if c.Generation != e.Generation || c.Local != e.Local || c.Base != old.Base || c.Remote != "" || c.Reason != model.ConflictRemoteDeleted || c.RemoteRevision != model.DeletedRevision(e.Token) || read(t, s, c.Draft) != wantDraft {
			t.Fatal(c)
		}
	}
	again, _, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || again.ID != chosen.ID {
		t.Fatal(again, err)
	}
	// A client refreshing the second ID can resolve that exact record even
	// though ObserveRemoteDeletion consistently returns the first preferred ID.
	if _, _, err = s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: other.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: chosen.RemoteRevision, Action: model.ResolveAcceptDelete}); err != nil {
		t.Fatal(err)
	}
	all, err = s.Conflicts(ctx)
	if err != nil || len(all) != 0 {
		t.Fatal(all, err)
	}
}

func TestRemoteDeletionSameNameTokenlessObservationKeepsUnknownDelete(t *testing.T) {
	s, _, _ := openTest(t)
	old := deletionCleanEntry(t, s)
	op := deletionNamespace(t, s, old)
	if err := s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	recreated := entry(t, s, old.Remote)
	if recreated.ID == old.ID || recreated.Token != "" {
		t.Fatal(recreated)
	}
	recreated, job := stage(t, s, recreated, "new local body")
	c, removed, err := s.ObserveRemoteDeletion(ctx, recreated)
	if err != nil || removed || c.ID != "" {
		t.Fatal(c, removed, err)
	}
	if err = s.AcknowledgeMissingDelete(ctx, op.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	work, err := s.NamespaceJournal(ctx, op.ID)
	if err != nil || !work.Dispatched || work.Step != "delete" {
		t.Fatal(work, err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != job.ID {
		t.Fatal(pending, err)
	}
}

func TestPrepareCreateWaitsForOverlappingDeleteAcknowledgment(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "dispatched_unknown"}[dispatched], func(t *testing.T) {
			s, _, _ := openTest(t)
			old := deletionCleanEntry(t, s)
			op := deletionNamespace(t, s, old)
			if dispatched {
				if err := s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
					t.Fatal(err)
				}
			}
			local := entry(t, s, old.Remote)
			local, job := stage(t, s, local, "recreated bytes")
			if _, _, err := s.Prepare(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordPlan(ctx, job.ID, nil, job.Local); err != nil {
				t.Fatal(err)
			}
			want := ErrBusy
			if dispatched {
				want = model.ErrUnknownResult
			}
			if _, err := s.PrepareCreate(ctx, job.ID, "parent", "deleted.bin"); !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
			creates, err := s.PendingCreates(ctx)
			if err != nil || len(creates) != 0 {
				t.Fatal(creates, err)
			}
			if got, err := s.Entry(ctx, local.ID); err != nil || got.Local != local.Local {
				t.Fatal(got, err)
			}
			if dispatched {
				if err = s.AcknowledgeMissingDelete(ctx, op.ID); !errors.Is(err, model.ErrUnknownResult) {
					t.Fatal(err)
				}
				if err = s.ConfirmNamespaceStep(ctx, op.ID, "delete"); err != nil {
					t.Fatal(err)
				}
				if err = s.AcknowledgeNamespaceAction(ctx, op.ID); err != nil {
					t.Fatal(err)
				}
			} else if err = s.AcknowledgeMissingDelete(ctx, op.ID); err != nil {
				t.Fatal(err)
			}
			created, err := s.PrepareCreate(ctx, job.ID, "parent", "deleted.bin")
			if err != nil || created.EntryID != local.ID || created.Input != local.Local {
				t.Fatal(created, err)
			}
		})
	}
}
