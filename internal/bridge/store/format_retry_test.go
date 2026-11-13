package store

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
	"strings"
	"testing"
)

func formatConflict(t *testing.T, uncertain bool) (*Store, model.Entry, model.Job, model.Conflict) {
	t.Helper()
	s, _, _ := openTest(t)
	en, err := s.ObserveEntry(ctx, Observation{Remote: "note.md", Kind: model.KindDocx, Token: "document"})
	if err != nil {
		t.Fatal(err)
	}
	en, j := stage(t, s, en, "local body")
	j, en, err = s.Prepare(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if uncertain {
		if err = s.RecordPlan(ctx, j.ID, []model.Operation{{ID: "unconfirmed", Kind: model.OperationBlockReplace}}, en.Local); err != nil {
			t.Fatal(err)
		}
	}
	c := model.Conflict{EntryID: en.ID, Generation: en.Generation, Base: j.BaseLocal, Local: en.Local, Remote: en.Local, RemoteRevision: "1", Draft: en.Local, Reason: "unsupported_content"}
	if err = s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	en, err = s.Entry(ctx, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := s.Conflicts(ctx)
	if err != nil || len(cs) != 1 {
		t.Fatal(cs, err)
	}
	return s, en, j, cs[0]
}
func TestFormatRetryRejectsDraftSavedAfterCapture(t *testing.T) {
	s, en, j, c := formatConflict(t, false)
	if err := s.SaveDraft(ctx, c.ID, strings.NewReader("newer human draft")); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RetryCompatibilityConflict(ctx, c)
	cs, _ := s.Conflicts(ctx)
	job, _ := s.Job(ctx, j.ID)
	current, _ := s.Entry(ctx, en.ID)
	if err != nil || changed || len(cs) != 1 || read(t, s, cs[0].Draft) != "newer human draft" || job.State != model.StatusConflict || current.Local != en.Local {
		t.Fatal("stale retry overwrote draft", changed, err, cs, job)
	}
}
func TestFormatRetryPreservesUncertainOperationJournal(t *testing.T) {
	s, en, j, c := formatConflict(t, true)
	before, _ := s.Journal(ctx, j.ID)
	changed, err := s.RetryCompatibilityConflict(ctx, c)
	after, _ := s.Journal(ctx, j.ID)
	cs, _ := s.Conflicts(ctx)
	current, _ := s.Entry(ctx, en.ID)
	if changed || !errors.Is(err, model.ErrUnknownResult) || len(cs) != 1 || after.Job.Plan != before.Job.Plan || len(after.Operations) != 1 || current != en {
		t.Fatal("unknown operation lost", changed, err, before, after, current)
	}
}
func TestFormatRetryRejectsNewerLocalGeneration(t *testing.T) {
	s, en, _, c := formatConflict(t, false)
	newer, _ := stage(t, s, en, "newest local body")
	changed, err := s.RetryCompatibilityConflict(ctx, c)
	current, _ := s.Entry(ctx, en.ID)
	if err != nil || changed || current.Local != newer.Local || current.Generation != newer.Generation {
		t.Fatal("new local save overwritten", changed, err, current, newer)
	}
}
func TestFormatRetryRetainsCapturesWithoutHumanAuthority(t *testing.T) {
	s, en, j, c := formatConflict(t, false)
	if err := s.update(context.Background(), func(tx *bolt.Tx) error {
		r, e := jobTx(tx, j.ID)
		if e != nil {
			return e
		}
		r.HumanResolution = true
		return saveJob(tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RetryCompatibilityConflict(ctx, c)
	after, _ := s.Journal(ctx, j.ID)
	current, _ := s.Entry(ctx, en.ID)
	cs, _ := s.Conflicts(ctx)
	if err != nil || !changed || after.HumanResolution || current.Local != en.Local || current.Generation != en.Generation || len(cs) != 0 || after.Job.State != model.StatusQueued {
		t.Fatal("bad compatibility transition", changed, err, after, current, cs)
	}
	if read(t, s, c.Local) != "local body" || read(t, s, c.Draft) != "local body" {
		t.Fatal("captured content lost")
	}
	events, err := s.HistoryEvents(ctx, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	retained := false
	for _, e := range events {
		if e.Conflict != nil && e.Conflict.ID == c.ID && e.Conflict.Resolved && e.Bodies["conflict_local"] == c.Local && e.Bodies["draft"] == c.Draft {
			retained = true
		}
	}
	if !retained {
		t.Fatal("retry did not retain immutable captures in history")
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestObsoleteFormatConflictYieldsToNewestSave(t *testing.T) {
	s, en, old, c := formatConflict(t, false)
	latest, newJob := stage(t, s, en, "latest local content")
	if err := s.SupersedeUnsentPredecessors(ctx, en.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != newJob.ID {
		t.Fatal("old format job still blocks latest", pending, err)
	}
	conflicts, _ := s.Conflicts(ctx)
	current, _ := s.Entry(ctx, en.ID)
	r, _ := s.Journal(ctx, old.ID)
	if len(conflicts) != 0 || current.Local != latest.Local || current.Generation != latest.Generation || r.Job.State != model.StatusConflict || read(t, s, c.Local) != "local body" {
		t.Fatal("obsolete capture lost or falsely synced", conflicts, current, r)
	}
}
func TestObsoleteFormatRetirementKeepsDraftsAndUncertainWork(t *testing.T) {
	for _, name := range []string{"edited draft", "uncertain operation", "genuine conflict", "creation evidence"} {
		t.Run(name, func(t *testing.T) {
			s, en, j, c := formatConflict(t, name == "uncertain operation")
			switch name {
			case "edited draft":
				if err := s.SaveDraft(ctx, c.ID, strings.NewReader("manual work")); err != nil {
					t.Fatal(err)
				}
			case "genuine conflict":
				c.Reason = "overlapping_edits"
				if err := s.SaveConflict(ctx, c); err != nil {
					t.Fatal(err)
				}
			case "creation evidence":
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					return put(tx, "createops", "legacy-create", model.CreateOp{ID: "legacy-create", JobID: j.ID, EntryID: en.ID, Generation: en.Generation, Kind: model.KindDocx, State: "prepared"})
				}); err != nil {
					t.Fatal(err)
				}
			}
			latest, _ := stage(t, s, en, "newer local content")
			if err := s.SupersedeUnsentPredecessors(ctx, en.ID); err != nil {
				t.Fatal(err)
			}
			cs, _ := s.Conflicts(ctx)
			pending, _ := s.Pending(ctx)
			current, _ := s.Entry(ctx, en.ID)
			if len(cs) != 1 || len(pending) != 2 || current.Local != latest.Local {
				t.Fatal("protected work retired", cs, pending, current)
			}
			if name == "edited draft" && read(t, s, cs[0].Draft) != "manual work" {
				t.Fatal("draft overwritten")
			}
		})
	}
}

func TestEquivalentDraftRetryStillGuardsConcurrentHumanEdit(t *testing.T) {
	s, _, j, c := formatConflict(t, false)
	if err := s.SaveDraft(ctx, c.ID, strings.NewReader("old<br>draft")); err != nil {
		t.Fatal(err)
	}
	captures, _ := s.Conflicts(ctx)
	expected := captures[0]
	if err := s.SaveDraft(ctx, c.ID, strings.NewReader("new human draft")); err != nil {
		t.Fatal(err)
	}
	changed, err := s.RetryEquivalentDraftConflict(ctx, expected, []byte("same"), []byte("same"))
	now, _ := s.Conflicts(ctx)
	job, _ := s.Job(ctx, j.ID)
	if err != nil || changed || len(now) != 1 || read(t, s, now[0].Draft) != "new human draft" || job.State != model.StatusConflict {
		t.Fatal("new draft consumed", changed, err, now, job)
	}
}
func TestEquivalentDraftRetryRejectsDifferentNormalizedBodies(t *testing.T) {
	s, _, _, c := formatConflict(t, false)
	changed, err := s.RetryEquivalentDraftConflict(ctx, c, []byte("local"), []byte("draft"))
	if err != nil || changed {
		t.Fatal("non-equivalent proof accepted", changed, err)
	}
}
