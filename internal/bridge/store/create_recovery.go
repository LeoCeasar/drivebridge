package store

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

var ErrCreateNeedsAttention = errors.New("creation needs attention")

func createForJobTx(tx *bolt.Tx, id model.JobID) (out model.CreateOp, err error) {
	found := false
	err = each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
		if op.JobID != id || op.State == "not_dispatched" {
			return nil
		}
		if found {
			return ErrInvalidState
		}
		out = op
		found = true
		return nil
	})
	if err == nil && !found {
		err = ErrNotFound
	}
	return
}

// CreateForJob returns the retained creation journal, including archived evidence.
func (s *Store) CreateForJob(ctx context.Context, id model.JobID) (out model.CreateOp, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error { out, err = createForJobTx(tx, id); return err })
	return
}

func pendingCreate(op model.CreateOp) bool {
	return op.State == "prepared" || op.State == "unknown" || op.State == "bound" || op.State == "renaming"
}
func creationCode(code string) string {
	if code == "" || len(code) > 64 || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return "unknown_result"
	}
	return code
}
func creationReason(reason string) string {
	switch reason {
	case "not_found", "multiple_matches", "content_mismatch", "network_or_protocol", "request_timeout", "permission_denied", "rename_unconfirmed", "location_changed":
		return reason
	default:
		return "network_or_protocol"
	}
}

// MarkCreateUncertain retains the first classified failure and any returned
// identity. Partial results are evidence; they are never authority to bind.
func (s *Store) MarkCreateUncertain(ctx context.Context, id, candidateToken, candidateRevision, code string, now time.Time) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if err := get(tx, "createops", id, &op); err != nil {
			return err
		}
		if !pendingCreate(op) {
			return ErrInvalidState
		}
		if candidateToken != "" && (op.CandidateToken != "" && op.CandidateToken != candidateToken || op.Token != "" && op.Token != candidateToken) {
			return model.ErrIdentityChanged
		}
		if candidateRevision != "" && op.CandidateRevision != "" && op.CandidateRevision != candidateRevision {
			return model.ErrIdentityChanged
		}
		j, err := jobTx(tx, op.JobID)
		if err != nil {
			return err
		}
		if j.Superseded || j.LocalReceipt {
			return ErrInvalidState
		}
		if op.FailureCode == "" {
			op.FailureCode = creationCode(code)
		}
		if op.FirstUncertainAt.IsZero() {
			op.FirstUncertainAt = now.UTC()
		}
		if candidateToken != "" {
			op.CandidateToken = candidateToken
		}
		if candidateRevision != "" {
			op.CandidateRevision = candidateRevision
		}
		if op.State == "prepared" {
			op.State = "unknown"
		}
		if err = put(tx, "createops", id, op); err != nil {
			return err
		}
		return saveCreateCheckState(tx, op, j)
	})
}

func saveCreateCheckState(tx *bolt.Tx, op model.CreateOp, j jobRecord) error {
	j.Job.State = model.StatusAwaitingVerification
	j.ErrorCode = "creation_unconfirmed"
	j.ResumeState = ""
	j.PreparationRestart = false
	if op.NeedsAttention {
		j.Job.State = model.StatusBlocked
		j.ErrorCode = "creation_needs_attention"
	}
	if err := saveJob(tx, j); err != nil {
		return err
	}
	en, err := entryTx(tx, op.EntryID)
	if err != nil {
		return err
	}
	if en.Generation == op.Generation {
		en.Status = j.Job.State
		return saveEntry(tx, en)
	}
	return nil
}

// RecordCreateCheck records a failed observation and stops automatic checking
// after three failures or fifteen minutes. It never changes dispatch phase.
func (s *Store) RecordCreateCheck(ctx context.Context, id, reason, code string, now time.Time) (out model.CreateOp, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		if er := get(tx, "createops", id, &out); er != nil {
			return er
		}
		if !pendingCreate(out) {
			return ErrInvalidState
		}
		j, er := jobTx(tx, out.JobID)
		if er != nil {
			return er
		}
		if j.Superseded || j.LocalReceipt {
			return ErrInvalidState
		}
		if out.FailureCode == "" {
			out.FailureCode = "unknown_result"
		}
		if out.FirstUncertainAt.IsZero() {
			out.FirstUncertainAt = j.StagedAt
			if out.FirstUncertainAt.IsZero() {
				out.FirstUncertainAt = now.UTC()
			}
		}
		out.CheckAttempts++
		out.LastCheckedAt = now.UTC()
		out.CheckReason = creationReason(reason)
		out.NeedsAttention = out.NeedsAttention || out.CheckAttempts >= 3 || now.Sub(out.FirstUncertainAt) >= 15*time.Minute
		if er = put(tx, "createops", id, out); er != nil {
			return er
		}
		return saveCreateCheckState(tx, out, j)
	})
	return
}

func recoveryCreateTx(tx *bolt.Tx, req model.CreateRecoveryRequest) (en model.Entry, j jobRecord, op model.CreateOp, err error) {
	j, err = jobTx(tx, req.JobID)
	if err != nil {
		return
	}
	if j.Superseded || j.LocalReceipt {
		err = ErrInvalidState
		return
	}
	op, err = createForJobTx(tx, req.JobID)
	if err != nil {
		return
	}
	if !pendingCreate(op) || op.EntryID != j.Job.EntryID || op.Generation != j.Job.Generation || op.Input != j.Job.Local {
		err = ErrInvalidState
		return
	}
	en, err = entryTx(tx, j.Job.EntryID)
	if err != nil {
		return
	}
	if en.Generation != req.ExpectedGeneration {
		err = model.ErrGenerationChanged
		return
	}
	if en.Local == "" || en.Local != req.ExpectedLocal {
		err = model.ErrVersionChanged
		return
	}
	if en.Kind != op.Kind || en.MountID == "" || en.ProfileID == "" {
		err = model.ErrIdentityChanged
		return
	}
	if en.FrozenBy != "" {
		err = ErrFrozen
		return
	}
	var m Metadata
	err = get(tx, "metadata", string(en.ID), &m)
	if err != nil {
		return
	}
	if m.Tombstone {
		err = ErrNotFound
		return
	}
	if m.FuseSoftRemove != "" || m.SafeSavePending != "" || m.SafeSaveParked || managedEntryPending(tx, en.ID) {
		err = ErrFrozen
		return
	}
	if j.Job.State != model.StatusAwaitingVerification && !(j.Job.State == model.StatusBlocked && op.NeedsAttention && j.ErrorCode == "creation_needs_attention") {
		err = ErrInvalidState
	}
	return
}

// RecheckCreate grants observation only. It preserves both budget and mutation
// phase so an unsuccessful manual check can immediately require attention again.
func (s *Store) RecheckCreate(ctx context.Context, req model.CreateRecoveryRequest) (out model.Entry, err error) {
	if req.Action != "recheck" {
		return out, ErrInvalidState
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		en, j, op, er := recoveryCreateTx(tx, req)
		if er != nil {
			return er
		}
		op.NeedsAttention = false
		if er = put(tx, "createops", op.ID, op); er != nil {
			return er
		}
		if er = saveCreateCheckState(tx, op, j); er != nil {
			return er
		}
		out, er = entryTx(tx, en.ID)
		return er
	})
	return
}

// RecreateCreate archives uncertain evidence and stages a separate, explicitly
// authorized creation using the latest durable local input in one transaction.
func (s *Store) RecreateCreate(ctx context.Context, req model.CreateRecoveryRequest) (out model.Entry, newJob model.Job, err error) {
	if req.Action != "recreate" || !req.ConfirmRecreate {
		return out, newJob, ErrInvalidState
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		en, j, op, er := recoveryCreateTx(tx, req)
		if er != nil {
			return er
		}
		if req.ExpectedRemote != "" && en.Remote != req.ExpectedRemote {
			return model.ErrVersionChanged
		}
		if req.ExpectedParentToken != "" {
			currentParentToken, err := createParentTokenTx(tx, en.Remote)
			if err != nil {
				return err
			}
			if currentParentToken != req.ExpectedParentToken {
				return model.ErrVersionChanged
			}
		}
		if !op.NeedsAttention || op.Token != "" || op.CandidateToken != "" || en.Token != "" || en.RemoteCheckpoint != "" || en.LocalCheckpoint != "" {
			return ErrInvalidState
		}
		if er = s.checkBlob(en.Local); er != nil {
			return er
		}
		if er = each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if c.EntryID == en.ID && !c.Resolved {
				return ErrBusy
			}
			return nil
		}); er != nil {
			return er
		}
		if er = each[model.CreateOp](tx, "createops", func(other model.CreateOp) error {
			if other.EntryID != en.ID || other.ID == op.ID || other.State == "not_dispatched" || other.State == "recreated" {
				return nil
			}
			return ErrBusy
		}); er != nil {
			return er
		}
		if er = each[jobRecord](tx, "jobs", func(other jobRecord) error {
			if other.Job.EntryID != en.ID || other.Job.ID == j.Job.ID || other.Superseded || !pending(other.Job.State) {
				return nil
			}
			if !unsentJob(other) {
				return model.ErrUnknownResult
			}
			return nil
		}); er != nil {
			return er
		}
		j.Superseded = true
		j.Job.State = model.StatusBlocked
		j.ResumeState = ""
		if er = saveJob(tx, j); er != nil {
			return er
		}
		// The retired job retains its desired output. The replacement has not
		// planned an output yet and must not inherit that old current-entry hint.
		en.Desired = ""
		if er = saveEntry(tx, en); er != nil {
			return er
		}
		out, newJob, er = s.stageBlob(tx, en.ID, &req.ExpectedGeneration, en.Local)
		if er != nil {
			return er
		}
		op.State = "recreated"
		op.ReplacedBy = newJob.ID
		return put(tx, "createops", op.ID, op)
	})
	return
}

// clearCreateFailure removes only transient recovery presentation after trusted
// verification. The original failure, budget, and dispatch phase remain evidence.
func clearCreateFailure(tx *bolt.Tx, op *model.CreateOp, en *model.Entry) error {
	j, err := jobTx(tx, op.JobID)
	if err != nil {
		return err
	}
	if j.Superseded || j.LocalReceipt {
		return ErrInvalidState
	}
	op.NeedsAttention = false
	op.CheckReason = ""
	if j.ErrorCode != "creation_unconfirmed" && j.ErrorCode != "creation_needs_attention" {
		return nil
	}
	j.ErrorCode = ""
	j.Diagnostic = nil
	j.ResumeState = ""
	j.Job.State = model.StatusAwaitingVerification
	if en.Generation == op.Generation {
		en.Status = j.Job.State
	}
	return saveJob(tx, j)
}

// createParentTokenTx resolves the current namespace inside the recreation CAS.
func createParentTokenTx(tx *bolt.Tx, remote string) (string, error) {
	parent := path.Dir(remote)
	if parent == "." || parent == "" {
		var scope Scope
		if err := get(tx, "meta", "scope", &scope); err != nil {
			return "", err
		}
		return scope.FolderToken, nil
	}
	en, err := byPath(tx, parent)
	if errors.Is(err, ErrNotFound) {
		return "", model.ErrVersionChanged
	}
	if err != nil {
		return "", err
	}
	if en.Kind != model.KindFolder || en.Token == "" {
		return "", model.ErrVersionChanged
	}
	var metadata Metadata
	if err = get(tx, "metadata", string(en.ID), &metadata); err != nil {
		return "", err
	}
	if metadata.Tombstone {
		return "", model.ErrVersionChanged
	}
	return en.Token, nil
}
