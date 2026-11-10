package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func (s *Store) PrepareCreate(ctx context.Context, jobID model.JobID, parentToken, finalName string) (out model.CreateOp, err error) {
	if parentToken == "" || finalName == "" || strings.ContainsAny(finalName, "/\\\x00") || finalName == "." || finalName == ".." {
		return out, errors.New("invalid creation target")
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		found := false
		if er := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
			if op.JobID == jobID {
				out = op
				found = true
			}
			return nil
		}); er != nil {
			return er
		}
		if found {
			if out.ParentToken != parentToken || out.FinalName != finalName {
				return ErrInvalidState
			}
			return nil
		}
		j, er := jobTx(tx, jobID)
		if er != nil {
			return er
		}
		e, er := entryTx(tx, j.Job.EntryID)
		if er != nil {
			return er
		}
		if e.FrozenBy != "" {
			return ErrFrozen
		}
		if e.Token != "" {
			return model.ErrIdentityChanged
		}
		if !active(j.Job.State) || j.Job.State == model.StatusAwaitingVerification {
			return ErrInvalidState
		}
		if j.Job.Local == "" {
			return errors.New("create input must be durable")
		}
		if er = s.checkBlob(j.Job.Local); er != nil {
			return er
		}
		id := uuid.NewString()
		out = model.CreateOp{ID: id, JobID: jobID, EntryID: e.ID, Generation: j.Job.Generation, Kind: e.Kind, ParentToken: parentToken, TemporaryName: ".drivebridge-" + id, FinalName: finalName, Input: j.Job.Local, State: "prepared"}
		return put(tx, "createops", id, out)
	})
	return
}
func (s *Store) MarkCreateUnknown(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if er := get(tx, "createops", id, &op); er != nil {
			return er
		}
		if op.State == "unknown" {
			return nil
		}
		if op.State != "prepared" {
			return ErrInvalidState
		}
		op.State = "unknown"
		if er := put(tx, "createops", id, op); er != nil {
			return er
		}
		j, er := jobTx(tx, op.JobID)
		if er != nil {
			return er
		}
		j.Job.State = model.StatusAwaitingVerification
		if er = saveJob(tx, j); er != nil {
			return er
		}
		e, er := entryTx(tx, op.EntryID)
		if er != nil {
			return er
		}
		if e.Generation == op.Generation {
			e.Status = model.StatusAwaitingVerification
			return saveEntry(tx, e)
		}
		return nil
	})
}
func (s *Store) BindCreated(ctx context.Context, id, token, revision string) (out model.CreateOp, err error) {
	if token == "" {
		return out, errors.New("empty created token")
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		if er := get(tx, "createops", id, &out); er != nil {
			return er
		}
		if out.Token != "" {
			if out.Token != token || out.Revision != revision {
				return model.ErrIdentityChanged
			}
			return nil
		}
		if out.State != "prepared" && out.State != "unknown" {
			return ErrInvalidState
		}
		e, er := entryTx(tx, out.EntryID)
		if er != nil {
			return er
		}
		if e.Token != "" && e.Token != token {
			return model.ErrIdentityChanged
		}
		e.Token = token
		out.Token = token
		out.Revision = revision
		out.State = "bound"
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		return put(tx, "createops", id, out)
	})
	return
}
func (s *Store) FinishCreate(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if er := get(tx, "createops", id, &op); er != nil {
			return er
		}
		if op.State == "finished" {
			return nil
		}
		if op.State != "bound" {
			return ErrInvalidState
		}
		e, er := entryTx(tx, op.EntryID)
		if er != nil {
			return er
		}
		if e.Token != op.Token {
			return model.ErrIdentityChanged
		}
		op.State = "finished"
		return put(tx, "createops", id, op)
	})
}
func (s *Store) PendingCreates(ctx context.Context) (out []model.CreateOp, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
			if op.State != "finished" {
				out = append(out, op)
			}
			return nil
		})
	})
	return
}
