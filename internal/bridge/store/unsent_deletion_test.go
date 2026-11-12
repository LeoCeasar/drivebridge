package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func legacyUnsentDeletion(t *testing.T, s *Store) (model.Entry, model.Job, model.Conflict) {
	t.Helper()
	e, job := stage(t, s, entry(t, s, "unsent.txt"), "saved local bytes")
	c := model.Conflict{ID: "legacy-delete", EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "delete_vs_pending_edit"}
	if err := s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	return e, job, c
}

func TestObserveUnsentDeletionPreservesLegacyChoicesAndDrafts(t *testing.T) {
	for _, action := range []string{model.ResolveRestoreLocal, model.ResolveAcceptDelete} {
		t.Run(action, func(t *testing.T) {
			s, dir, scope := openTest(t)
			e, _, old := legacyUnsentDeletion(t, s)
			if err := s.SaveDraft(ctx, old.ID, strings.NewReader("old user draft")); err != nil {
				t.Fatal(err)
			}
			e, _ = stage(t, s, e, "latest accepted local bytes")
			other := old
			other.ID = "second-delete"
			other.Generation = e.Generation
			other.Local = e.Local
			if err := s.SaveConflict(ctx, other); err != nil {
				t.Fatal(err)
			}
			if err := s.SaveDraft(ctx, other.ID, strings.NewReader("second user draft")); err != nil {
				t.Fatal(err)
			}
			c, err := s.ObserveUnsentDeletion(ctx, e)
			if err != nil || c.ID != old.ID || c.Reason != model.ConflictLocalDeletePending || c.RemoteRevision != model.LocalDeletionRevision(e.ID, e.Generation) || c.Local != e.Local || c.Remote != "" {
				t.Fatal(c, err)
			}
			all, err := s.Conflicts(ctx)
			if err != nil || len(all) != 2 {
				t.Fatal(all, err)
			}
			for _, captured := range all {
				draft := "old user draft"
				if captured.ID == other.ID {
					draft = "second user draft"
				}
				if captured.Reason != model.ConflictLocalDeletePending || captured.Generation != e.Generation || captured.Local != e.Local || captured.RemoteRevision != c.RemoteRevision || read(t, s, captured.Draft) != draft {
					t.Fatal(captured)
				}
			}
			again, err := s.ObserveUnsentDeletion(ctx, e)
			if err != nil || again.ID != c.ID {
				t.Fatal(again, err)
			}
			if _, _, err = s.Resolve(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("implicit merge")}); !errors.Is(err, ErrInvalidState) {
				t.Fatal(err)
			}
			r := model.ResolveRequest{ConflictID: other.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: action}
			stale := r
			stale.ExpectedRemoteRevision = "stale"
			if _, _, err = s.ResolveRemoteDeletion(ctx, stale); !errors.Is(err, model.ErrVersionChanged) {
				t.Fatal(err)
			}
			result, job, err := s.ResolveRemoteDeletion(ctx, r)
			if err != nil || result.Generation != e.Generation+1 || result.Token != "" || result.LocalCheckpoint != "" || result.RemoteCheckpoint != "" || result.Local != e.Local {
				t.Fatal(result, job, err)
			}
			pending, err := s.Pending(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if action == model.ResolveRestoreLocal {
				if len(pending) != 1 || pending[0].ID != job.ID || job.State != model.StatusQueued {
					t.Fatal(pending, job)
				}
			} else {
				if len(pending) != 0 || job.ID != "" {
					t.Fatal(pending, job)
				}
				if _, err = s.EntryByRemote(ctx, e.Remote); !errors.Is(err, ErrNotFound) {
					t.Fatal(err)
				}
			}
			all, err = s.Conflicts(ctx)
			if err != nil || len(all) != 0 {
				t.Fatal(all, err)
			}
			actions, err := s.NamespaceActions(ctx)
			if err != nil || len(actions) != 0 {
				t.Fatal(actions, err)
			}
			if err = s.Validate(ctx); err != nil {
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
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
			if read(t, s, result.Local) != "latest accepted local bytes" {
				t.Fatal("local recovery lost")
			}
		})
	}
}

func TestObserveUnsentDeletionRejectsUnknownAndStaleState(t *testing.T) {
	for _, change := range []string{"generation", "local", "binding", "checkpoint", "unknown", "create"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := openTest(t)
			e, job, c := legacyUnsentDeletion(t, s)
			want := model.ErrVersionChanged
			switch change {
			case "generation":
				e.Generation++
				want = model.ErrGenerationChanged
			case "local":
				e.Local = "stale"
			case "binding":
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					latest, err := entryTx(tx, e.ID)
					if err != nil {
						return err
					}
					latest.Token = "bound"
					return saveEntry(tx, latest)
				}); err != nil {
					t.Fatal(err)
				}
				want = ErrInvalidState
			case "checkpoint":
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					latest, err := entryTx(tx, e.ID)
					if err != nil {
						return err
					}
					latest.LocalCheckpoint = e.Local
					return saveEntry(tx, latest)
				}); err != nil {
					t.Fatal(err)
				}
				want = ErrInvalidState
			case "unknown":
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					rec, err := jobTx(tx, job.ID)
					if err != nil {
						return err
					}
					rec.Job.State = model.StatusAwaitingVerification
					return saveJob(tx, rec)
				}); err != nil {
					t.Fatal(err)
				}
				want = model.ErrUnknownResult
			case "create":
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					return put(tx, "createops", "unknown-create", model.CreateOp{ID: "unknown-create", EntryID: e.ID, JobID: job.ID, State: "unknown"})
				}); err != nil {
					t.Fatal(err)
				}
				want = model.ErrUnknownResult
			}
			if _, err := s.ObserveUnsentDeletion(ctx, e); !errors.Is(err, want) {
				t.Fatalf("want %v, got %v", want, err)
			}
			all, err := s.Conflicts(ctx)
			if err != nil || len(all) != 1 || all[0].ID != c.ID || all[0].Reason != c.Reason {
				t.Fatal(all, err)
			}
			pending, err := s.Pending(ctx)
			if err != nil || len(pending) != 1 || pending[0].ID != job.ID {
				t.Fatal(pending, err)
			}
		})
	}
}

func TestObserveUnsentDeletionRequiresLegacyDeleteConflict(t *testing.T) {
	s, _, _ := openTest(t)
	e, _ := stage(t, s, entry(t, s, "normal.txt"), "local")
	if _, err := s.ObserveUnsentDeletion(ctx, e); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if err := s.SaveConflict(ctx, model.Conflict{ID: "ordinary", EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "concurrent_edit"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveUnsentDeletion(ctx, e); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestUnsentDeletionRestoreUsesExplicitContentAndPreservesDraft(t *testing.T) {
	for _, body := range []string{"chosen edited bytes", ""} {
		t.Run(map[bool]string{false: "edited", true: "empty"}[body == ""], func(t *testing.T) {
			s, _, _ := openTest(t)
			e, _, legacy := legacyUnsentDeletion(t, s)
			if err := s.SaveDraft(ctx, legacy.ID, strings.NewReader("saved recovery draft")); err != nil {
				t.Fatal(err)
			}
			c, err := s.ObserveUnsentDeletion(ctx, e)
			if err != nil {
				t.Fatal(err)
			}
			chosen := []byte(body)
			restored, job, err := s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Action: model.ResolveRestoreLocal, Content: chosen})
			if err != nil || restored.Token != "" || job.Local != restored.Local || read(t, s, restored.Local) != body {
				t.Fatal(restored, job, err)
			}
			if err = s.view(ctx, func(tx *bolt.Tx) error {
				var saved model.Conflict
				if err := get(tx, "conflicts", string(c.ID), &saved); err != nil {
					return err
				}
				if !saved.Resolved || saved.Draft != c.Draft || saved.Local != e.Local {
					t.Fatal(saved)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if read(t, s, c.Draft) != "saved recovery draft" {
				t.Fatal("draft overwritten")
			}
		})
	}
}

func TestUnsentDeletionResolveRejectsUnknownWorkAfterCapture(t *testing.T) {
	s, _, _ := openTest(t)
	e, job, _ := legacyUnsentDeletion(t, s)
	c, err := s.ObserveUnsentDeletion(ctx, e)
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
	all, err := s.Conflicts(ctx)
	if err != nil || len(all) != 1 || all[0].Reason != model.ConflictLocalDeletePending {
		t.Fatal(all, err)
	}
}
