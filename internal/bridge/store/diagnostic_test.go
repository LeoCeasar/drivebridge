package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestPauseDiagnosticSurvivesRestartAndClearsOnResume(t *testing.T) {
	s, dir, scope := openTest(t)
	en := entry(t, s, "draft.md")
	_, job := stage(t, s, en, "first\n<div>bad</div>\n")
	if _, _, err := s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	diagnostic := &model.SyncDiagnostic{Kind: "html_block", Message: "HTML block cannot be written back", Line: 2, EndLine: 2, Text: "<div>bad</div>", Side: "local"}
	if err := s.PauseJobWithDiagnostic(ctx, job.ID, model.StatusBlocked, "unsupported_content", diagnostic); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	journal, err := reopened.Journal(ctx, job.ID)
	if err != nil || journal.ErrorCode != "unsupported_content" || !reflect.DeepEqual(journal.Diagnostic, diagnostic) {
		t.Fatalf("diagnostic lost: %+v %v", journal, err)
	}
	if err := reopened.ResumeJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	journal, err = reopened.Journal(ctx, job.ID)
	if err != nil || journal.ErrorCode != "" || journal.Diagnostic != nil {
		t.Fatalf("stale diagnostic after resume: %+v %v", journal, err)
	}
}

func TestPauseDiagnosticCannotHideUnknownMutation(t *testing.T) {
	s, _, _ := openTest(t)
	_, job := stage(t, s, entry(t, s, "draft.md"), "draft")
	if _, _, err := s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PauseJobWithDiagnostic(ctx, job.ID, model.StatusBlocked, "unsupported_content", &model.SyncDiagnostic{Kind: "html_block", Line: 1}); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	journal, err := s.Journal(ctx, job.ID)
	if err != nil || journal.Job.State != model.StatusAwaitingVerification || journal.Diagnostic != nil || journal.ErrorCode != "" {
		t.Fatalf("unknown mutation changed: %+v %v", journal, err)
	}
}
