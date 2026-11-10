package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// CancelUnsentLocalFile implements an explicit unlink of a never-bound local
// file. The eligibility check, journal retention, supersession and tombstone are
// one transaction, serialized with Prepare/RecordPlan/PrepareCreate. False leaves
// the existing remote/pending-edit deletion policy in charge.
func (s *Store) CancelUnsentLocalFile(ctx context.Context, id model.EntryID, generation uint64) (cancelled bool, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		e, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return model.ErrGenerationChanged
		}
		var metadata Metadata
		if err := get(tx, "metadata", string(id), &metadata); err != nil {
			return err
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		if (e.Kind != model.KindFile && e.Kind != model.KindDocx) || e.Token != "" || e.RemoteCheckpoint != "" || e.LocalCheckpoint != "" || e.Desired != "" || e.FrozenBy != "" || metadata.VirtualType != "" || e.Status == model.StatusConflict || namespaceReserved(tx, e.Remote) {
			return nil
		}
		safe := true
		var jobs []jobRecord
		if err := each[jobRecord](tx, "jobs", func(j jobRecord) error {
			if j.Job.EntryID == id {
				jobs = append(jobs, j)
				if !unsentJob(j) {
					safe = false
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
			if op.EntryID == id {
				safe = false
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each[publication](tx, "publications", func(p publication) error {
			if p.Candidate.EntryID == id {
				safe = false
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
			if (r.ActionPending || r.Dispatched) && (r.Op.SrcID == id || r.Op.DstID == id || r.Op.ResultID == id) {
				safe = false
			}
			return nil
		}); err != nil {
			return err
		}
		if !safe {
			return nil
		}
		sequence, err := tx.Bucket([]byte("namespace")).NextSequence()
		if err != nil {
			return err
		}
		operation := model.NamespaceOp{ID: uuid.NewString(), Kind: "delete", Src: e.Remote, SrcID: id, ResultID: id, ExpectedSrc: e.Generation, Input: e.Local, State: "finished"}
		record := namespaceRecord{Ordinal: sequence, Op: operation, Before: []model.Entry{e}, Sealed: map[model.EntryID]uint64{id: e.Generation}, LocalCancel: true}
		if err := put(tx, "namespace", operation.ID, record); err != nil {
			return err
		}
		if err := put(tx, "recovery", "namespace:"+operation.ID, recoveryRecord{NamespaceID: operation.ID, Entries: []model.Entry{e}}); err != nil {
			return err
		}
		for _, j := range jobs {
			j.Superseded = true
			if err := saveJob(tx, j); err != nil {
				return err
			}
		}
		if err := tombstone(tx, e); err != nil {
			return err
		}
		cancelled = true
		return nil
	})
	return
}
