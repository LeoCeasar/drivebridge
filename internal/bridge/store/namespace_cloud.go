package store

import (
	"context"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

type NamespaceWork struct {
	SafeSaveBackup    bool
	SafeSaveCompleted bool
	LocalReceipt      bool
	LocalCancel       bool
	TemporaryName     string

	Op         model.NamespaceOp
	Before     []model.Entry
	Step       string
	Dispatched bool
	Completed  map[string]bool
}

func (s *Store) NamespaceJournal(ctx context.Context, id string) (out NamespaceWork, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		out = NamespaceWork{SafeSaveBackup: r.SafeSaveBackup, SafeSaveCompleted: r.SafeSaveCompleted, LocalReceipt: r.LocalReceipt, LocalCancel: r.LocalCancel, TemporaryName: r.TemporaryName, Op: r.Op, Before: r.Before, Step: r.CloudStep, Dispatched: r.Dispatched, Completed: r.CompletedSteps}
		return nil
	})
	return
}
func (s *Store) BeginNamespaceDispatch(ctx context.Context, id, step string) error {
	switch step {
	case "move", "rename", "delete", "create_folder", "cleanup_source":
	default:
		return ErrInvalidState
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.SafeSaveBackup || r.FuseSoftRemove {
			return ErrSafeSavePending
		}
		if r.WaitFor != "" {
			var parent namespaceRecord
			if err := get(tx, "namespace", r.WaitFor, &parent); err != nil {
				return err
			}
			if parent.Op.State != "finished" {
				return ErrFrozen
			}
		}
		if !r.ActionPending || r.Op.State != "finished" || r.CompletedSteps[step] {
			return ErrInvalidState
		}
		if r.Dispatched {
			return model.ErrUnknownResult
		}
		blocked, err := managedNamespaceBlockedTx(tx, r)
		if err != nil {
			return err
		}
		if blocked {
			return ErrBusy
		}
		r.CloudStep = step
		r.Dispatched = true
		return put(tx, "namespace", id, r)
	})
}
func (s *Store) ConfirmNamespaceStep(ctx context.Context, id, step string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.CompletedSteps[step] {
			return nil
		}
		if !r.ActionPending || !r.Dispatched || r.CloudStep != step {
			return ErrInvalidState
		}
		if r.CompletedSteps == nil {
			r.CompletedSteps = map[string]bool{}
		}
		r.CompletedSteps[step] = true
		r.Dispatched = false
		return put(tx, "namespace", id, r)
	})
}
func (s *Store) BindNamespaceFolder(ctx context.Context, id, token string) error {
	if token == "" {
		return model.ErrIdentityChanged
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.Op.Kind != "mkdir" || r.Op.State != "finished" || !r.ActionPending {
			return ErrInvalidState
		}
		en, err := entryTx(tx, r.Op.ResultID)
		if err != nil {
			return err
		}
		if en.Token != "" && en.Token != token {
			return model.ErrIdentityChanged
		}
		en.Token = token
		return saveEntry(tx, en)
	})
}
func (s *Store) ProtectNamespaceBackup(ctx context.Context, id string, snapshotID model.SnapshotID) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		var sn snapshotRecord
		if err := get(tx, "snapshots", string(snapshotID), &sn); err != nil {
			return err
		}
		found := false
		for _, en := range r.Before {
			if en.Token == sn.Snapshot.Token {
				found = true
			}
		}
		if !found {
			return model.ErrIdentityChanged
		}
		return put(tx, "recovery", "namespace-backup:"+id+":"+string(snapshotID), recoveryRecord{NamespaceID: id, SnapshotID: snapshotID})
	})
}
func (s *Store) MarkCreateRenaming(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if err := get(tx, "createops", id, &op); err != nil {
			return err
		}
		if op.State == "renaming" {
			return model.ErrUnknownResult
		}
		if op.State != "bound" {
			return ErrInvalidState
		}
		op.State = "renaming"
		return put(tx, "createops", id, op)
	})
}

// RejectNamespaceDispatch requires a trusted pre-dispatch guard rejection, never
// a timeout or CLI failure after the request may have reached the service.
func (s *Store) RejectNamespaceDispatch(ctx context.Context, id, step string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if !r.ActionPending || !r.Dispatched || r.CloudStep != step {
			return ErrInvalidState
		}
		r.Dispatched = false
		return put(tx, "namespace", id, r)
	})
}
func (s *Store) RejectCreateRename(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if err := get(tx, "createops", id, &op); err != nil {
			return err
		}
		if op.State != "renaming" || op.Token == "" {
			return ErrInvalidState
		}
		op.State = "bound"
		return put(tx, "createops", id, op)
	})
}
