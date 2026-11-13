package store

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// RetryCompatibilityConflict requeues only a captured converter failure whose
// draft is still the exact local input. It grants no human-resolution authority,
// retains all immutable inputs and never erases an uncertain operation journal.
func (s *Store) RetryCompatibilityConflict(ctx context.Context, expected model.Conflict) (bool, error) {
	if expected.Reason != "unsupported_content" {
		return false, nil
	}
	return s.retryTextConflict(ctx, expected, false, false)
}

// RetryCleanTextConflict is invoked only after a clean Git merge of the captured
// inputs. The transaction still owns the generation, draft and journal guards.
func (s *Store) RetryCleanTextConflict(ctx context.Context, expected model.Conflict) (bool, error) {
	switch expected.Reason {
	case "overlapping_edits", "ambiguous_mapping", "same_insertion", "table_both_changed", "delete_vs_edit", "merge_complexity", "remote_drift", "unsupported_content":
		return s.retryTextConflict(ctx, expected, true, false)
	}
	return false, nil
}

// RetryEquivalentDraftConflict requires exact normalized equality computed by
// the background converter. Original captures remain immutable; the transaction
// still checks the complete expected record so a newer human draft cannot race.
func (s *Store) RetryEquivalentDraftConflict(ctx context.Context, expected model.Conflict, local, draft []byte) (bool, error) {
	if len(local) > 10<<20 || len(draft) > 10<<20 || !bytes.Equal(local, draft) {
		return false, nil
	}
	switch expected.Reason {
	case "overlapping_edits", "ambiguous_mapping", "same_insertion", "table_both_changed", "delete_vs_edit", "merge_complexity", "remote_drift", "unsupported_content":
		return s.retryTextConflict(ctx, expected, true, true)
	}
	return false, nil
}
func (s *Store) retryTextConflict(ctx context.Context, expected model.Conflict, plain, equivalentDraft bool) (changed bool, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if err := get(tx, "conflicts", string(expected.ID), &c); err != nil {
			return err
		}
		if c != expected || c.Resolved || c.Local == "" || c.Draft != "" && c.Draft != c.Local && !equivalentDraft {
			return nil
		}
		en, err := entryTx(tx, c.EntryID)
		if err != nil {
			return err
		}
		if (en.Kind != model.KindDocx && !(plain && model.IsMarkdownFile(en))) || en.Generation != c.Generation || en.Local != c.Local || en.FrozenBy != "" {
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

// retireObsoleteFormatConflicts runs only in a background sync transaction.
// Local saves do not perform this scan. A wholly undispatched format failure
// can yield to a newer local save, but edited drafts and uncertain journals
// keep their original exclusion and immutable inputs.
func retireObsoleteFormatConflicts(tx *bolt.Tx, en model.Entry) error {
	if en.Kind != model.KindDocx {
		return nil
	}
	var metadata Metadata
	if err := get(tx, "metadata", string(en.ID), &metadata); err != nil {
		return err
	}
	if metadata.Tombstone {
		return nil
	}
	groups := map[uint64][]model.Conflict{}
	if err := each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
		if c.EntryID == en.ID && !c.Resolved && c.Generation < en.Generation {
			groups[c.Generation] = append(groups[c.Generation], c)
		}
		return nil
	}); err != nil {
		return err
	}
	for generation, conflicts := range groups {
		safe := true
		for _, c := range conflicts {
			safe = safe && c.Reason == "unsupported_content" && (c.Draft == "" || c.Draft == c.Local)
		}
		if !safe {
			continue
		}
		var records []jobRecord
		if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
			if r.Job.EntryID != en.ID || r.Job.Generation != generation || r.Superseded {
				return nil
			}
			if r.Job.State != model.StatusConflict || r.Job.Plan != "" || len(r.Job.OperationIDs) != 0 || len(r.Operations) != 0 || len(r.Results) != 0 || r.Desired != "" || r.PreparationRestart {
				safe = false
			}
			records = append(records, r)
			return nil
		}); err != nil {
			return err
		}
		if !safe || len(records) == 0 {
			continue
		}
		creates := false
		if err := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
			for _, r := range records {
				if op.JobID == r.Job.ID {
					creates = true
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if creates {
			continue
		}
		for _, r := range records {
			r.Superseded = true
			if err := saveJob(tx, r); err != nil {
				return err
			}
		}
		for _, c := range conflicts {
			c.Resolved = true
			if err := put(tx, "conflicts", string(c.ID), c); err != nil {
				return err
			}
		}
	}
	return nil
}
