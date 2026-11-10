package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestAppendStepRequiresPreviousReadback(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "draft.md")
	_, j := stage(t, s, e, "new")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	op := model.Operation{Kind: model.OperationBlockReplace, Route: "verified", CapabilityID: "cap", Payload: json.RawMessage(`{}`)}
	if err := s.RecordPlan(ctx, j.ID, []model.Operation{op}, j.Local); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOperation(ctx, j.ID, op, j.Local); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if err := s.RecordOperation(ctx, j.ID, 0, json.RawMessage(`{"verified":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOperation(ctx, j.ID, op, j.Local); err != nil {
		t.Fatal(err)
	}
	journal, err := s.Journal(ctx, j.ID)
	if err != nil || len(journal.Operations) != 2 || journal.Operations[0].ID == journal.Operations[1].ID {
		t.Fatalf("%+v %v", journal, err)
	}
}

func TestPauseResumeCannotReplayUnknown(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "draft.bin")
	_, j := stage(t, s, e, "bytes")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseJob(ctx, j.ID, model.StatusBlocked, "permission_denied"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseJob(ctx, j.ID, model.StatusPaused, "unknown_result"); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	if err := s.ResumeJob(ctx, j.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	got, err := s.Journal(ctx, j.ID)
	if err != nil || got.Job.State != model.StatusAwaitingVerification {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestNamespaceDispatchIsDurableAndNeverBlindlyReplayed(t *testing.T) {
	s, dir, scope := openTest(t)
	e := entry(t, s, "old.bin")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "new.bin"})
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
	if err = s.BeginNamespaceDispatch(ctx, op.ID, "rename"); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginNamespaceDispatch(ctx, op.ID, "rename"); !errors.Is(err, model.ErrUnknownResult) {
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
	work, err := s.NamespaceJournal(ctx, op.ID)
	if err != nil || !work.Dispatched || work.Step != "rename" {
		t.Fatalf("%+v %v", work, err)
	}
	if err = s.ConfirmNamespaceStep(ctx, op.ID, "rename"); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginNamespaceDispatch(ctx, op.ID, "rename"); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
}

func TestNamespaceActionsKeepLocalOrder(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "a")
	var ids []string
	for _, next := range []string{"b", "c", "d"} {
		op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: next})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SealNamespace(ctx, op.ID); err != nil {
			t.Fatal(err)
		}
		e, err = s.CommitNamespace(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishNamespace(ctx, op.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, op.ID)
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != len(ids) {
		t.Fatal(actions, err)
	}
	for i := range ids {
		if actions[i].ID != ids[i] {
			t.Fatal("namespace order lost")
		}
	}
}
