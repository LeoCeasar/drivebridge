package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// AppendOperation persists the next remapped operation only after every earlier
// operation has a durable readback result. It never erases the original journal.
func (s *Store) AppendOperation(ctx context.Context, id model.JobID, op model.Operation, desired model.BlobID) error {
	before, err := s.Journal(ctx, id)
	if err != nil {
		return err
	}
	if err = s.checkBlob(desired); err != nil {
		return err
	}
	if op.ID == "" {
		op.ID = uuid.NewString()
	}
	ops := append(append([]model.Operation(nil), before.Operations...), op)
	encoded, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	b, err := s.PutBlob(ctx, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		if r.Job.Plan != before.Job.Plan || len(r.Operations) != len(before.Operations) {
			return model.ErrVersionChanged
		}
		if r.Job.State != model.StatusUploading && r.Job.State != model.StatusAwaitingVerification {
			return ErrInvalidState
		}
		for i := range r.Operations {
			if _, ok := r.Results[i]; !ok {
				return ErrInvalidState
			}
		}
		for _, old := range r.Operations {
			if old.ID == op.ID {
				return ErrInvalidState
			}
		}
		r.Operations = ops
		r.Job.OperationIDs = append(r.Job.OperationIDs, op.ID)
		r.Job.Plan = b
		r.Desired = desired
		r.Job.State = model.StatusUploading
		e, err := entryTx(tx, r.Job.EntryID)
		if err != nil {
			return err
		}
		if e.Generation == r.Job.Generation {
			e.Desired = desired
			e.Status = r.Job.State
			if err = saveEntry(tx, e); err != nil {
				return err
			}
		}
		return saveJob(tx, r)
	})
}

// PauseJob is permitted only before dispatch or after all dispatched operations
// have readback records. An unknown mutation stays awaiting_verification.
func (s *Store) PauseJob(ctx context.Context, id model.JobID, state model.Status, code string) error {
	return s.PauseJobWithDiagnostic(ctx, id, state, code, nil)
}

// PauseJobWithDiagnostic atomically records the failure and captured source detail.
// It has the same dispatch safety requirements as PauseJob.
func (s *Store) PauseJobWithDiagnostic(ctx context.Context, id model.JobID, state model.Status, code string, diagnostic *model.SyncDiagnostic) error {
	if state != model.StatusBlocked && state != model.StatusPaused {
		return ErrInvalidState
	}
	if code == "" || len(code) > 64 || strings.Trim(code, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return ErrInvalidState
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		if r.Job.State == model.StatusAwaitingVerification && r.PreparationRestart {
			r.Job.State = model.StatusMerging
			r.PreparationRestart = false
		}
		if r.Job.State == model.StatusAwaitingVerification {
			return model.ErrUnknownResult
		}
		if r.Job.State != model.StatusMerging && r.Job.State != model.StatusUploading {
			return ErrInvalidState
		}
		if r.Job.State == model.StatusUploading {
			for i := range r.Operations {
				if _, ok := r.Results[i]; !ok {
					return model.ErrUnknownResult
				}
			}
		}
		r.ResumeState = r.Job.State
		r.ErrorCode = code
		r.Diagnostic = diagnostic
		r.Job.State = state
		e, err := entryTx(tx, r.Job.EntryID)
		if err != nil {
			return err
		}
		if e.Generation == r.Job.Generation {
			e.Status = state
			if err = saveEntry(tx, e); err != nil {
				return err
			}
		}
		return saveJob(tx, r)
	})
}

func (s *Store) ResumeJob(ctx context.Context, id model.JobID) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		if r.Job.State == model.StatusAwaitingVerification {
			return model.ErrUnknownResult
		}
		if r.Superseded || (r.Job.State != model.StatusPaused && r.Job.State != model.StatusBlocked) {
			return ErrInvalidState
		}
		if r.ResumeState == model.StatusMerging {
			r.Job.State = model.StatusQueued
		} else if r.ResumeState == model.StatusUploading {
			r.Job.State = model.StatusUploading
		} else {
			return ErrInvalidState
		}
		r.ErrorCode = ""
		r.Diagnostic = nil
		r.ResumeState = ""
		e, err := entryTx(tx, r.Job.EntryID)
		if err != nil {
			return err
		}
		if e.Generation == r.Job.Generation {
			e.Status = r.Job.State
			if err = saveEntry(tx, e); err != nil {
				return err
			}
		}
		return saveJob(tx, r)
	})
}

// ResetUndispatched may only be called with a trusted transport guard's proof
// that no CLI child started. It cannot reset a journaled document mutation.
func (s *Store) ResetUndispatched(ctx context.Context, id model.JobID) error {
	return s.update(ctx, func(tx *bolt.Tx) error { return resetUndispatched(tx, id) })
}
func resetUndispatched(tx *bolt.Tx, id model.JobID) error {
	r, err := jobTx(tx, id)
	if err != nil {
		return err
	}
	if r.Job.State != model.StatusUploading || len(r.Operations) != 0 {
		return ErrInvalidState
	}
	r.Job.State = model.StatusMerging
	r.Job.Plan = ""
	r.Desired = ""
	en, err := entryTx(tx, r.Job.EntryID)
	if err != nil {
		return err
	}
	if en.Generation == r.Job.Generation {
		en.Desired = ""
		en.Status = r.Job.State
		if err = saveEntry(tx, en); err != nil {
			return err
		}
	}
	return saveJob(tx, r)
}
func (s *Store) RejectUndispatchedCreate(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if err := get(tx, "createops", id, &op); err != nil {
			return err
		}
		if op.State != "prepared" || op.Token != "" {
			return ErrInvalidState
		}
		op.State = "not_dispatched"
		if err := put(tx, "createops", id, op); err != nil {
			return err
		}
		return resetUndispatched(tx, op.JobID)
	})
}
