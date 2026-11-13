package syncer

import (
	"context"
	"errors"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

// Uncertainty is not a reason to replay a mutation. Record bounded observations
// using a durable context, including when the request that failed was canceled.
func (e *Engine) failedCreateCheck(ctx context.Context, op model.CreateOp, reason string, cause error) error {
	updated, err := e.store.RecordCreateCheck(context.WithoutCancel(ctx), op.ID, reason, creationFailureCode(cause), e.now())
	if err != nil {
		return errors.Join(cause, err)
	}
	if updated.NeedsAttention {
		return errors.Join(store.ErrCreateNeedsAttention, cause)
	}
	return errors.Join(model.ErrUnknownResult, cause)
}
func creationFailureCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "request_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var api *larkcli.Error
	if errors.As(err, &api) {
		return classify(api)
	}
	if errors.Is(err, larkcli.ErrProtocol) {
		return "network_or_protocol"
	}
	if err == nil {
		return "unknown_result"
	}
	return classify(err)
}

var errCreationContentMismatch = errors.New("created content differs from captured input")

func creationCheckReason(err error) string {
	if errors.Is(err, errCreationContentMismatch) {
		return "content_mismatch"
	}
	switch creationFailureCode(err) {
	case "permission_denied", "request_timeout":
		return creationFailureCode(err)
	}
	return "network_or_protocol"
}

// RecoverCreation accepts one explicit recovery for one captured local version.
// Recreate is a new operation, not an assertion that the old one never happened.
func (e *Engine) RecoverCreation(ctx context.Context, req model.CreateRecoveryRequest) (model.CreateRecoveryResult, error) {
	out := model.CreateRecoveryResult{}
	if req.JobID == "" || req.ExpectedLocal == "" || req.ExpectedGeneration == 0 || (req.Action != "recheck" && req.Action != "recreate") || req.Action == "recreate" && !req.ConfirmRecreate {
		return out, model.ErrUnsupportedContent
	}
	ctx, cancel := context.WithTimeout(ctx, e.opt.InteractiveTimeout)
	defer cancel()
	defer e.beginForeground()()
	if err := e.check(ctx); err != nil {
		return out, err
	}
	op, err := e.store.CreateForJob(ctx, req.JobID)
	if err != nil {
		return out, recoveryVersionError(err)
	}
	unlock, err := e.entryLock(ctx, op.EntryID)
	if err != nil {
		return out, err
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	op, err = e.store.CreateForJob(ctx, req.JobID)
	if err != nil {
		return out, recoveryVersionError(err)
	}
	en, err := e.store.Entry(ctx, op.EntryID)
	if err != nil {
		return out, recoveryVersionError(err)
	}
	if en.Generation != req.ExpectedGeneration {
		return out, model.ErrGenerationChanged
	}
	if en.Local != req.ExpectedLocal {
		return out, model.ErrVersionChanged
	}
	if req.Action == "recheck" {
		if op.State == "recreated" || op.State == "finished" || op.State == "not_dispatched" {
			return out, model.ErrVersionChanged
		}
		en, err = e.store.RecheckCreate(ctx, req)
		if err != nil {
			return out, recoveryVersionError(err)
		}
		unlock()
		locked = false
		if err = e.enqueueCreationRecovery(ctx, en); err != nil {
			return out, err
		}
		return model.CreateRecoveryResult{EntryID: en.ID, JobID: req.JobID, Generation: en.Generation, Accepted: true}, nil
	}
	if !op.NeedsAttention {
		return out, store.ErrCreateNeedsAttention
	}
	if (op.State != "unknown" && op.State != "prepared") || op.Token != "" || op.CandidateToken != "" || en.Token != "" {
		return out, model.ErrVersionChanged
	}
	parent, err := e.parentToken(ctx, en.Remote)
	if err != nil {
		return out, err
	}
	if parent != op.ParentToken {
		return out, model.ErrVersionChanged
	}
	items, err := e.source.List(ctx, parent)
	if err != nil {
		return out, err
	}
	// Names of other resource kinds also block recreation; never choose a
	// same-name file merely because its displayed content looks equivalent.
	final := cloudName(en)
	for _, item := range items {
		if item.Name == op.TemporaryName || item.Name == op.FinalName || item.Name == final {
			return out, model.ErrVersionChanged
		}
	}
	if e.deps.BeforeCreateRecovery == nil {
		return out, history.ErrGitUnavailable
	}
	if err = e.deps.BeforeCreateRecovery(ctx, en); err != nil {
		return out, err
	}
	req.ExpectedRemote = en.Remote
	req.ExpectedParentToken = parent
	en, job, err := e.store.RecreateCreate(ctx, req)
	if err != nil {
		return out, recoveryVersionError(err)
	}
	unlock()
	locked = false
	if err = e.enqueueCreationRecovery(ctx, en); err != nil {
		return out, err
	}
	return model.CreateRecoveryResult{EntryID: en.ID, JobID: job.ID, Generation: en.Generation, Recreated: true, Accepted: true}, nil
}
func recoveryVersionError(err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidState) {
		return model.ErrVersionChanged
	}
	return err
}
func (e *Engine) enqueueCreationRecovery(ctx context.Context, en model.Entry) error {
	e.dispatchMu.RLock()
	d := e.dispatcher
	e.dispatchMu.RUnlock()
	if d != nil {
		return d.manual(ctx, en.ID, en.Generation)
	}
	// A standalone engine does not own a service lifetime. Durable queued work is
	// left for its explicit SyncEntry/SyncNow call, never a detached goroutine.
	return nil
}
