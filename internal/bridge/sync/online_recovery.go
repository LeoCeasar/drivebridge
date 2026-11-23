package syncer

import (
	"context"
	"errors"
	"time"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/store"
)

func (e *Engine) retainJobFailure(ctx context.Context, id model.JobID, cause error, previousID string) {
	j, err := e.store.Journal(context.WithoutCancel(ctx), id)
	if err != nil {
		return
	}
	if j.Online != nil && j.Online.Last != nil && j.Online.Last.ID != previousID {
		return
	}
	outcome := "unknown"
	if j.Job.State == model.StatusAwaitingVerification {
		outcome = "dispatched_unconfirmed"
	}
	f := online.Capture(ctx, cause, "update", "unknown", outcome, e.now())
	if errors.Is(cause, store.ErrSpace) {
		f.Cause = "insufficient_space"
		f.Action = "repair_local"
	}
	if errors.Is(cause, history.ErrGitUnavailable) {
		f.Cause = "history_git_unavailable"
		f.Action = "repair_local"
	}
	if errors.Is(cause, errParentPending) {
		f.Cause = "parent_pending"
		f.Action = "wait_dependency"
	}
	_ = e.store.RecordJobOnlineFailure(context.WithoutCancel(ctx), id, f)
}

func (e *Engine) recordEntryOnline(ctx context.Context, id model.EntryID, cause error, phase string) {
	jobs, err := e.store.Pending(context.WithoutCancel(ctx))
	if err != nil {
		return
	}
	for _, j := range jobs {
		if j.EntryID == id {
			outcome := "not_dispatched"
			if j.State == model.StatusAwaitingVerification {
				outcome = "dispatched_unconfirmed"
			}
			_ = e.store.RecordJobOnlineFailure(context.WithoutCancel(ctx), j.ID, online.Capture(ctx, cause, "sync", phase, outcome, e.now()))
			return
		}
	}
}

// observePendingCreation has a read-only cloud capability. It returns after
// observation even when a normal pass will later have an original rename left.
func (e *Engine) observePendingCreation(ctx context.Context, j model.Job, op model.CreateOp) error {
	var body []byte
	var err error
	if op.Kind == model.KindDocx {
		body, err = e.read(ctx, op.Input)
		if err != nil {
			return e.failedCreateCheck(ctx, op, "local_state_corrupt", online.AtPhase(err, "create", "local_commit"))
		}
	}
	observed, err := ObserveCreation(ctx, e.source, op, body)
	if err == nil {
		err = online.AtPhase(e.check(ctx), "create", "identity_check")
	}
	if err != nil {
		if observed.Token != "" && op.CandidateToken == "" && op.Token == "" {
			if saveErr := e.store.MarkCreateUncertain(context.WithoutCancel(ctx), op.ID, observed.Token, "", op.FailureCode, e.now()); saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
		reason := observed.Reason
		if reason == "" {
			reason = creationCheckReason(err)
		}
		return e.failedCreateCheck(ctx, op, reason, err)
	}
	if !observed.Verified {
		return model.ErrUnknownResult
	}
	// Candidate evidence is not ownership. The original executor re-verifies it
	// under its normal admission before BindCreated/remaining mutations.
	return e.store.RecordCreationObserved(ctx, op.ID, observed.Token, observed.Revision)
}

func (e *Engine) creationObservationDue(ctx context.Context, j model.Job) (bool, error) {
	op, err := e.store.CreateForJob(ctx, j.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if op.NeedsAttention || op.State == "finished" || op.State == "recreated" || op.State == "not_dispatched" {
		return false, nil
	}
	if op.ObservationOnly || j.State == model.StatusAwaitingVerification && !op.ObservationReady {
		if e.now().Before(op.NextCheckAt) {
			return true, nil
		}
		return true, e.observePendingCreation(ctx, j, op)
	}
	return false, nil
}

func createObservationFailure(ctx context.Context, cause error, reason string, now time.Time) model.OnlineFailure {
	f := online.Capture(ctx, cause, "create", "readback", "dispatched_unconfirmed", now)
	switch reason {
	case "local_state_corrupt":
		if f.Cause == "unclassified_error" {
			f.Cause = "local_state_corrupt"
		}
		f.Action = "repair_local"
	case "not_found":
		f.Cause = "resource_missing_unconfirmed"
		f.Action = "observe_only"
	case "multiple_matches":
		f.Cause = "multiple_candidates"
		f.Action = "review_conflict"
	case "content_mismatch":
		f.Cause = "content_mismatch"
		f.Action = "review_conflict"
	case "location_changed":
		f.Cause = "resource_location_changed"
		f.Action = "review_conflict"
	}
	return f
}
