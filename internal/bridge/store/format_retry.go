package store

import (
	"context"
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// RetryCompatibilityConflict requeues only a captured converter failure whose
// draft is still the exact local input. It grants no human-resolution authority,
// retains all immutable inputs and never erases an uncertain operation journal.
func (s *Store) RetryCompatibilityConflict(ctx context.Context, expected model.Conflict) (changed bool, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if err := get(tx, "conflicts", string(expected.ID), &c); err != nil {
			return err
		}
		if c != expected || c.Resolved || c.Reason != "unsupported_content" || c.Local == "" || c.Draft != "" && c.Draft != c.Local {
			return nil
		}
		en, err := entryTx(tx, c.EntryID)
		if err != nil {
			return err
		}
		if en.Kind != model.KindDocx || en.Generation != c.Generation || en.Local != c.Local || en.FrozenBy != "" {
			return nil
		}
		var metadata Metadata
		if err = get(tx, "metadata", string(en.ID), &metadata); err != nil {
			return err
		}
		if metadata.Tombstone {
			return nil
		}
		other := false
		if err = each[model.Conflict](tx, "conflicts", func(x model.Conflict) error {
			if x.ID != c.ID && x.EntryID == c.EntryID && !x.Resolved {
				other = true
			}
			return nil
		}); err != nil {
			return err
		}
		if other {
			return nil
		}
		var selected *jobRecord
		if err = each[jobRecord](tx, "jobs", func(j jobRecord) error {
			if j.Job.EntryID == en.ID && j.Job.Generation == en.Generation && j.Job.State == model.StatusConflict && !j.Superseded {
				if selected != nil {
					return ErrInvalidState
				}
				copy := j
				selected = &copy
			}
			return nil
		}); err != nil {
			return err
		}
		if selected == nil || selected.Job.Local != c.Local || selected.Job.BaseLocal != c.Base {
			return nil
		}
		r := *selected
		if r.PreparationRestart || len(r.Job.OperationIDs) != len(r.Operations) {
			return model.ErrUnknownResult
		}
		for i := range r.Operations {
			var receipt struct {
				AfterSnapshot model.SnapshotID `json:"after_snapshot"`
			}
			if json.Unmarshal(r.Results[i], &receipt) != nil || receipt.AfterSnapshot == "" {
				return model.ErrUnknownResult
			}
		}
		r.Job.State = model.StatusQueued
		if len(r.Operations) > 0 {
			r.Job.State = model.StatusUploading
		}
		r.ErrorCode = ""
		r.ResumeState = ""
		r.HumanResolution = false
		if err = saveJob(tx, r); err != nil {
			return err
		}
		en.Status = r.Job.State
		if err = saveEntry(tx, en); err != nil {
			return err
		}
		c.Resolved = true
		if err = put(tx, "conflicts", string(c.ID), c); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}
