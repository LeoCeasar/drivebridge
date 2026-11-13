package store

import (
	"context"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// ObserveUnsentDeletion upgrades an existing unresolved local unlink decision
// for never-bound content. It supplies no evidence of a cloud disappearance and
// never discards local bytes or creates a cloud namespace action.
func (s *Store) ObserveUnsentDeletion(ctx context.Context, expected model.Entry) (out model.Conflict, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		e, er := entryTx(tx, expected.ID)
		if er != nil {
			return er
		}
		var metadata Metadata
		if er = get(tx, "metadata", string(e.ID), &metadata); er != nil {
			return er
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		if (e.Kind != model.KindFile && e.Kind != model.KindDocx) || e.Token != "" || e.LocalCheckpoint != "" || e.RemoteCheckpoint != "" {
			return ErrInvalidState
		}
		if e.Generation != expected.Generation {
			return model.ErrGenerationChanged
		}
		if e.Token != expected.Token || e.Kind != expected.Kind || e.Remote != expected.Remote {
			return model.ErrIdentityChanged
		}
		if e.Local != expected.Local || e.LocalCheckpoint != expected.LocalCheckpoint || e.RemoteCheckpoint != expected.RemoteCheckpoint {
			return model.ErrVersionChanged
		}
		if metadata.VirtualType != "" || metadata.FuseSoftRemove != "" || metadata.SafeSavePending != "" || metadata.SafeSaveParked || metadata.SafeSaveOriginal != "" {
			return ErrFrozen
		}
		if er = deletionWorkSafe(tx, e); er != nil {
			return er
		}
		var conflicts []model.Conflict
		if er = each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if c.EntryID == e.ID && !c.Resolved && (c.Reason == "delete_vs_pending_edit" || c.Reason == model.ConflictLocalDeletePending) {
				conflicts = append(conflicts, c)
			}
			return nil
		}); er != nil {
			return er
		}
		if len(conflicts) == 0 {
			return ErrInvalidState
		}
		for i, c := range conflicts {
			c.Generation = e.Generation
			if e.Local != "" {
				c.Local = e.Local
			}
			c.Reason = model.ConflictLocalDeletePending
			c.Remote = ""
			c.RemoteRevision = model.LocalDeletionRevision(e.ID, e.Generation)
			if er = put(tx, "conflicts", string(c.ID), c); er != nil {
				return er
			}
			if i == 0 {
				out = c
			}
		}
		e.Status = model.StatusConflict
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		return each[jobRecord](tx, "jobs", func(r jobRecord) error {
			if r.Job.EntryID == e.ID && !r.Superseded && pending(r.Job.State) {
				r.Job.State = model.StatusConflict
				return saveJob(tx, r)
			}
			return nil
		})
	})
	return
}
