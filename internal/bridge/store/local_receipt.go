package store

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// ReconcileSystemMetadata upgrades legacy, never-dispatched OS metadata jobs to
// local receipts. It does not delete a file or claim that cloud accepted it.
// The journal/identity check and every receipt are one Store transaction.
func (s *Store) ReconcileSystemMetadata(ctx context.Context) (settled int, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		candidates := map[model.EntryID]bool{}
		if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
			if !r.Superseded && !r.LocalReceipt && r.Job.State != model.StatusSynced {
				candidates[r.Job.EntryID] = true
			}
			return nil
		}); err != nil {
			return err
		}
		for id := range candidates {
			e, err := entryTx(tx, id)
			if err != nil {
				return err
			}
			name := path.Base(e.Remote)
			if e.Kind != model.KindFile || !(name == ".DS_Store" || name == "Thumbs.db" || strings.HasPrefix(name, "._")) {
				continue
			}
			safe, err := eligibleLocalReceipt(tx, e, "")
			if err != nil {
				return err
			}
			if !safe {
				continue
			}
			var jobs []model.JobID
			if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
				if r.Job.EntryID == id && !r.LocalReceipt {
					jobs = append(jobs, r.Job.ID)
				}
				return nil
			}); err != nil {
				return err
			}
			for _, job := range jobs {
				if err := recordLocalReceipt(tx, job); err != nil {
					return err
				}
				settled++
			}
		}
		return nil
	})
	if err != nil {
		settled = 0
	}
	return
}

// Receipt eligibility is conservative across all history, including superseded
// jobs. Ambiguous cloud work and conflicts must remain in the visible outbox.
func eligibleLocalReceipt(tx *bolt.Tx, e model.Entry, namespace string) (bool, error) {
	if e.Token != "" || e.RemoteCheckpoint != "" || e.LocalCheckpoint != "" || e.Desired != "" || e.Status == model.StatusConflict || (e.FrozenBy != "" && e.FrozenBy != namespace) {
		return false, nil
	}
	var metadata Metadata
	if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return false, err
	}
	if metadata.Tombstone || metadata.VirtualType != "" {
		return false, nil
	}
	safe := true
	if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID == e.ID && !unsentJob(r) {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := each[model.CreateOp](tx, "createops", func(r model.CreateOp) error {
		if r.EntryID == e.ID {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := each[publication](tx, "publications", func(r publication) error {
		if r.Candidate.EntryID == e.ID {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := each[model.Conflict](tx, "conflicts", func(r model.Conflict) error {
		if r.EntryID == e.ID {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
		if r.Op.ID != namespace && (r.Op.SrcID == e.ID || r.Op.DstID == e.ID || r.Op.ResultID == e.ID) {
			if r.Dispatched || r.CloudStep != "" || len(r.CompletedSteps) != 0 || r.Op.State == "prepared" || r.Op.State == "sealed" || r.Op.State == "committed" {
				safe = false
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	return safe, nil
}

func recordLocalReceipt(tx *bolt.Tx, id model.JobID) error {
	r, err := jobTx(tx, id)
	if err != nil {
		return err
	}
	r.LocalReceipt = true
	r.Superseded = true
	return saveJob(tx, r)
}
func receiptLatestLocalJob(tx *bolt.Tx, e model.Entry) error {
	return each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID == e.ID && r.Job.Generation == e.Generation && r.Job.Local == e.Local {
			return recordLocalReceipt(tx, r.Job.ID)
		}
		return nil
	})
}
func promoteLocalReceipt(tx *bolt.Tx, e *model.Entry) error {
	receipt := false
	if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID == e.ID && r.Job.Generation == e.Generation && r.Job.Local == e.Local && r.LocalReceipt && r.Superseded {
			receipt = true
		}
		return nil
	}); err != nil {
		return err
	}
	if !receipt {
		return nil
	}
	e.Generation++
	e.Status = model.StatusQueued
	job := model.Job{ID: model.JobID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Local: e.Local, BaseLocal: e.LocalCheckpoint, BaseRemote: e.RemoteCheckpoint, State: model.StatusQueued}
	return saveJob(tx, jobRecord{Job: job, StagedAt: time.Now().UTC()})
}

// ConfigureNamespaceLocality persists classification before cache Flush/commit,
// so a crash cannot leave a renamed receipt permanently outside the outbox.
func (s *Store) ConfigureNamespaceLocality(ctx context.Context, id string, localOnly, promote map[model.EntryID]bool) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.Op.State != "prepared" || (r.Op.Kind != "rename" && r.Op.Kind != "replace") {
			return ErrInvalidState
		}
		allowed := map[model.EntryID]bool{}
		for _, e := range r.Before {
			allowed[e.ID] = true
		}
		for key := range localOnly {
			if !allowed[key] {
				return ErrInvalidState
			}
		}
		for key := range promote {
			if !allowed[key] {
				return ErrInvalidState
			}
		}
		r.ResultLocalOnly = localOnly
		r.PromoteLocalOnly = promote
		return put(tx, "namespace", id, r)
	})
}
func (s *Store) ConfigureLocalOnlyMkdir(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.Op.Kind != "mkdir" || r.Op.State != "prepared" {
			return ErrInvalidState
		}
		r.LocalReceipt = true
		return put(tx, "namespace", id, r)
	})
}

// Promoted directories use the existing mkdir journal. Parent-first ordinals
// bind their tokens before the engine dispatches their children's content jobs.
// WaitFor gates cloud admission until the original rename's cache reconciliation
// has finished; committed rename replay releases the same gate after a crash.
func queuePromotedFolders(tx *bolt.Tx, r namespaceRecord) error {
	var folders []model.Entry
	for id, promote := range r.PromoteLocalOnly {
		if !promote {
			continue
		}
		e, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		if e.Kind != model.KindFolder || e.Token != "" {
			continue
		}
		safe, err := eligibleLocalReceipt(tx, e, r.Op.ID)
		if err != nil {
			return err
		}
		if !safe {
			return ErrBusy
		}
		folders = append(folders, e)
	}
	sort.Slice(folders, func(i, j int) bool {
		a, b := strings.Count(folders[i].Remote, "/"), strings.Count(folders[j].Remote, "/")
		if a != b {
			return a < b
		}
		return folders[i].Remote < folders[j].Remote
	})
	for _, e := range folders {
		ordinal, err := tx.Bucket([]byte("namespace")).NextSequence()
		if err != nil {
			return err
		}
		id := uuid.NewString()
		op := model.NamespaceOp{ID: id, Kind: "mkdir", Dst: e.Remote, ResultID: e.ID, State: "finished"}
		intent := namespaceRecord{Ordinal: ordinal, Op: op, TemporaryName: "DriveBridge-pending-" + id, Before: []model.Entry{e}, ActionPending: true, WaitFor: r.Op.ID}
		if err := put(tx, "namespace", id, intent); err != nil {
			return err
		}
	}
	return nil
}
