package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/lib/larkcli"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func unknownCreateFixture(t *testing.T) (*store.Store, *Engine, *fixtureCloud, model.Entry, model.Job, model.CreateOp) {
	t.Helper()
	s, e, source, _ := setup(t, model.KindFile)
	en, err := s.EnsureEntry(bg, "new.py", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	en = stageContent(t, s, en, "original Python bytes")
	source.loseResponse = true
	if err = e.SyncEntry(bg, en.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatalf("unknown creation not retained: %v", err)
	}
	jobs, _ := s.Pending(bg)
	var job model.Job
	for _, j := range jobs {
		if j.EntryID == en.ID {
			job = j
		}
	}
	ops, _ := s.PendingCreates(bg)
	var op model.CreateOp
	for _, o := range ops {
		if o.JobID == job.ID {
			op = o
		}
	}
	if op.ID == "" {
		t.Fatal("no create journal")
	}
	return s, e, source, en, job, op
}
func TestMissingCreationStopsAfterVerificationBudget(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	source.items = nil
	e.opt.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	err := e.SyncEntry(bg, en.ID)
	got, _ := s.Job(bg, job.ID)
	current, _ := s.CreateForJob(bg, job.ID)
	if got.State != model.StatusBlocked || !current.NeedsAttention || current.CheckReason != "not_found" {
		t.Fatalf("still loops: state=%s op=%+v err=%v", got.State, current, err)
	}
	before := current.CheckAttempts
	_ = e.SyncEntry(bg, en.ID)
	again, _ := s.CreateForJob(bg, job.ID)
	if again.CheckAttempts != before || source.writes != 1 {
		t.Fatal("attention task retried or recreated automatically")
	}
	if current.Input != op.Input {
		t.Fatal("original input lost")
	}
}

func recoveryRequest(en model.Entry, job model.Job, action string) model.CreateRecoveryRequest {
	return model.CreateRecoveryRequest{JobID: job.ID, Action: action, ExpectedGeneration: en.Generation, ExpectedLocal: en.Local, ConfirmRecreate: action == "recreate"}
}
func requireAttention(t *testing.T, s *store.Store, e *Engine, source *fixtureCloud, en model.Entry, job model.Job) {
	t.Helper()
	source.items = nil
	e.opt.Now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	_ = e.SyncEntry(bg, en.ID)
	op, _ := s.CreateForJob(bg, job.ID)
	if !op.NeedsAttention {
		t.Fatal("not attention")
	}
}

func TestRecreateCreationUsesLatestBodyAndRetainsOldEvidence(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	requireAttention(t, s, e, source, en, job)
	en = stageContent(t, s, en, "latest Python bytes")
	archived := false
	e.deps.BeforeCreateRecovery = func(ctx context.Context, current model.Entry) error {
		archived = current.Local == en.Local && source.writes == 1
		return nil
	}
	result, err := e.RecoverCreation(bg, recoveryRequest(en, job, "recreate"))
	if err != nil {
		t.Fatal(err)
	}
	if !archived || !result.Recreated || !result.Accepted || result.JobID == job.ID {
		t.Fatalf("bad recovery %+v", result)
	}
	old, err := s.CreateForJob(bg, job.ID)
	if err != nil || old.State != "recreated" || old.Input != op.Input || old.ReplacedBy != result.JobID {
		t.Fatalf("lost original evidence: %+v %v", old, err)
	}
	if err = e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	if current.Status != model.StatusSynced || string(source.file) != "latest Python bytes" {
		t.Fatal("latest input not confirmed")
	}
}
func TestRecreateCreationRejectsCloudNameCandidateAndStaleInput(t *testing.T) {
	for _, kind := range []string{"final", "temporary", "candidate", "stale", "archive_failure", "parent_drift"} {
		t.Run(kind, func(t *testing.T) {
			s, e, source, en, job, op := unknownCreateFixture(t)
			requireAttention(t, s, e, source, en, job)
			req := recoveryRequest(en, job, "recreate")
			e.deps.BeforeCreateRecovery = func(context.Context, model.Entry) error { return nil }
			switch kind {
			case "final":
				source.items = []larkcli.DriveItem{{Name: op.FinalName, Token: "other", Type: "file"}}
			case "temporary":
				source.items = []larkcli.DriveItem{{Name: op.TemporaryName, Token: "other", Type: "docx"}}
			case "candidate":
				if err := s.MarkCreateUncertain(bg, op.ID, "candidate", "", "request_timeout", e.now()); err != nil {
					t.Fatal(err)
				}
			case "stale":
				stageContent(t, s, en, "a later save")
			case "archive_failure":
				e.deps.BeforeCreateRecovery = func(context.Context, model.Entry) error { return errors.New("git failed") }
			case "parent_drift":
				source.items = nil // store is bound to root; a changed scope cannot be adopted.
				e.deps.BeforeCreateRecovery = func(ctx context.Context, _ model.Entry) error {
					_, _, err := s.Stage(ctx, en.ID, nil, strings.NewReader("changed during preflight"))
					return err
				}
			}
			if _, err := e.RecoverCreation(bg, req); err == nil {
				t.Fatal("unsafe recreate accepted")
			}
			retained, _ := s.CreateForJob(bg, job.ID)
			if retained.State == "recreated" || source.writes != 1 {
				t.Fatal("old task retired or new cloud content written")
			}
		})
	}
}
func TestRecheckDoesNotReuploadAndCanRecoverFoundObject(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	items := append([]larkcli.DriveItem{}, source.items...)
	requireAttention(t, s, e, source, en, job)
	result, err := e.RecoverCreation(bg, recoveryRequest(en, job, "recheck"))
	if err != nil || !result.Accepted || result.Recreated {
		t.Fatalf("recheck %+v %v", result, err)
	}
	source.items = items
	if err = e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	if current.Status != model.StatusSynced || source.writes != 1 {
		t.Fatal("recheck replayed creation")
	}
	original, _ := s.CreateForJob(bg, job.ID)
	if original.State != "finished" || original.Input != op.Input {
		t.Fatal("original creation not reconciled")
	}
}
func TestPartialCandidateIsVerifiedBeforeBinding(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	if err := s.MarkCreateUncertain(bg, op.ID, "created-file", "", "request_timeout", e.now()); err != nil {
		t.Fatal(err)
	}
	// A candidate with the wrong body must not acquire entry ownership.
	source.file = []byte("different remote bytes")
	_ = e.SyncEntry(bg, en.ID)
	current, _ := s.Entry(bg, en.ID)
	if current.Token != "" {
		t.Fatal("unverified candidate bound")
	}
	found, _ := s.CreateForJob(bg, job.ID)
	if found.CheckReason != "content_mismatch" || found.CandidateToken != "created-file" {
		t.Fatalf("candidate evidence lost %+v", found)
	}
}

// A crash may leave the durable intent prepared before dispatch evidence exists.
func TestPreparedCreationCanRecoverAfterMissingObservation(t *testing.T) {
	s, e, source, _ := setup(t, model.KindFile)
	en, err := s.EnsureEntry(bg, "prepared.py", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	en = stageContent(t, s, en, "latest prepared bytes")
	jobs, _ := s.Pending(bg)
	var job model.Job
	for _, j := range jobs {
		if j.EntryID == en.ID {
			job = j
		}
	}
	if _, _, err = s.Prepare(bg, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPlan(bg, job.ID, nil, job.Local); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareCreate(bg, job.ID, "root", "prepared.py")
	if err != nil {
		t.Fatal(err)
	}
	requireAttention(t, s, e, source, en, job)
	e.deps.BeforeCreateRecovery = func(context.Context, model.Entry) error { return nil }
	result, err := e.RecoverCreation(bg, recoveryRequest(en, job, "recreate"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recreated || source.writes != 0 {
		t.Fatal("unexpected replay")
	}
	if err = e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	if current.Status != model.StatusSynced || source.writes != 1 || source.uploadedName == op.TemporaryName {
		t.Fatal("replacement not independently confirmed")
	}
}

type cancelAfterCreateCloud struct {
	*fixtureCloud
	cancel context.CancelFunc
}

func (c *cancelAfterCreateCloud) UploadFile(ctx context.Context, parent, token, name string, in io.Reader) (larkcli.UploadResult, error) {
	result, err := c.fixtureCloud.UploadFile(ctx, parent, token, name, in)
	c.cancel()
	return result, err
}
func TestCanceledCreationBindingRetainsReturnedCandidate(t *testing.T) {
	s, e, source, _ := setup(t, model.KindFile)
	en, err := s.EnsureEntry(bg, "cancel.py", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	en = stageContent(t, s, en, "saved bytes")
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	e.source = &cancelAfterCreateCloud{source, cancel}
	if err = e.SyncEntry(ctx, en.ID); err == nil {
		t.Fatal("cancellation not observed")
	}
	jobs, _ := s.Pending(bg)
	var op model.CreateOp
	for _, j := range jobs {
		if j.EntryID == en.ID {
			op, err = s.CreateForJob(bg, j.ID)
		}
	}
	if err != nil || op.CandidateToken != "created-file" || op.Token != "" {
		t.Fatalf("returned identity lost: %+v %v", op, err)
	}
	e.source = source
	if err = e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	if current.Status != model.StatusSynced || source.writes != 1 {
		t.Fatal("creation repeated rather than reconciled")
	}
}

func TestListedCreationCandidateSurvivesMismatchAndDisappearance(t *testing.T) {
	s, e, source, en, job, _ := unknownCreateFixture(t)
	source.file = []byte("a different body")
	_ = e.SyncEntry(bg, en.ID)
	op, _ := s.CreateForJob(bg, job.ID)
	if op.CandidateToken != "created-file" || op.Token != "" {
		t.Fatalf("observed candidate discarded: %+v", op)
	}
	requireAttention(t, s, e, source, en, job)
	e.deps.BeforeCreateRecovery = func(context.Context, model.Entry) error { return nil }
	if _, err := e.RecoverCreation(bg, recoveryRequest(en, job, "recreate")); err == nil {
		t.Fatal("known candidate bypassed after it disappeared")
	}
	if source.writes != 1 {
		t.Fatal("creation replayed")
	}
}
