package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

const PreparedBlobGrace = 24 * time.Hour
const SnapshotRetention = 30 * 24 * time.Hour
const MinimumSnapshots = 10

type GCOptions struct {
	Now              time.Time
	CleanBudgetBytes int64
}
type GCResult struct {
	RemovedBlobs                int
	RemovedBytes, RetainedBytes int64
	OverBudget                  bool
}

// GC removes only unreferenced data. The clean-cache budget never overrides durable references.
func (s *Store) GC(ctx context.Context, o GCOptions) (result GCResult, err error) {
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.CleanBudgetBytes == 0 {
		o.CleanBudgetBytes = CleanCacheBudget
	}
	if o.CleanBudgetBytes < 0 {
		return result, errors.New("negative clean cache budget")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound {
		return result, ErrUnbound
	}
	if s.closed {
		return result, bolt.ErrDatabaseNotOpen
	}
	if err = ctx.Err(); err != nil {
		return
	}
	refs := make(map[model.BlobID]bool)
	snrefs := make(map[model.SnapshotID]bool)
	mark := func(b model.BlobID) {
		if b != "" {
			refs[b] = true
		}
	}
	markEntry := func(e model.Entry) {
		mark(e.Local)
		mark(e.Visible)
		mark(e.LocalCheckpoint)
		mark(e.Desired)
		if e.RemoteCheckpoint != "" {
			snrefs[e.RemoteCheckpoint] = true
		}
	}
	for b := range s.leases {
		mark(b)
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		if er := historyProtectedTx(tx, mark); er != nil {
			return er
		}
		all := make(map[model.SnapshotID]snapshotRecord)
		groups := make(map[string][]snapshotRecord)
		if er := each[snapshotRecord](tx, "snapshots", func(r snapshotRecord) error {
			all[r.Snapshot.ID] = r
			groups[r.Snapshot.Token] = append(groups[r.Snapshot.Token], r)
			return nil
		}); er != nil {
			return er
		}
		for _, records := range groups {
			sort.Slice(records, func(i, j int) bool { return records[i].SavedAt.After(records[j].SavedAt) })
			for i, r := range records {
				if i < MinimumSnapshots || o.Now.Sub(r.SavedAt) <= SnapshotRetention {
					snrefs[r.Snapshot.ID] = true
				}
			}
		}
		if er := each[model.Entry](tx, "entries", func(e model.Entry) error {
			markEntry(e)
			var m Metadata
			if err := get(tx, "metadata", string(e.ID), &m); err != nil {
				return err
			}
			if m.HistorySnapshot != "" && m.HistoryMediaBody == e.Local {
				snrefs[m.HistorySnapshot] = true
			}
			return nil
		}); er != nil {
			return er
		}
		jobgroups := make(map[model.EntryID][]jobRecord)
		if er := each[jobRecord](tx, "jobs", func(j jobRecord) error { jobgroups[j.Job.EntryID] = append(jobgroups[j.Job.EntryID], j); return nil }); er != nil {
			return er
		}
		for _, jobs := range jobgroups {
			sort.Slice(jobs, func(i, j int) bool { return jobs[i].Job.Generation > jobs[j].Job.Generation })
			for i, j := range jobs {
				if !j.CompletedAt.IsZero() && i >= MinimumSnapshots && o.Now.Sub(j.CompletedAt) > SnapshotRetention {
					if er := tx.Bucket([]byte("jobs")).Delete([]byte(j.Job.ID)); er != nil {
						return er
					}
					continue
				}
				mark(j.Job.Local)
				mark(j.Job.BaseLocal)
				mark(j.Job.Plan)
				mark(j.Desired)
				if j.Job.BaseRemote != "" {
					snrefs[j.Job.BaseRemote] = true
				}
			}
		}
		if er := each[model.Conflict](tx, "conflicts", func(c model.Conflict) error { mark(c.Base); mark(c.Local); mark(c.Remote); mark(c.Draft); return nil }); er != nil {
			return er
		}
		if er := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
			mark(r.Op.Input)
			mark(r.Op.Previous)
			for _, e := range r.Before {
				markEntry(e)
			}
			return nil
		}); er != nil {
			return er
		}
		if er := each[model.CreateOp](tx, "createops", func(r model.CreateOp) error { mark(r.Input); return nil }); er != nil {
			return er
		}
		if er := each[recoveryRecord](tx, "recovery", func(r recoveryRecord) error {
			if r.SnapshotID != "" {
				snrefs[r.SnapshotID] = true
			}
			for _, e := range r.Entries {
				markEntry(e)
			}
			return nil
		}); er != nil {
			return er
		}
		if er := each[publication](tx, "publications", func(p publication) error {
			e, er := entryTx(tx, p.Candidate.EntryID)
			if er != nil {
				return er
			}
			if e.Generation != p.Candidate.ExpectedGeneration || e.RemoteCheckpoint != p.Candidate.ExpectedRemote {
				return tx.Bucket([]byte("publications")).Delete([]byte(candidateKey(p.Candidate)))
			}
			snrefs[p.Candidate.NewRemote] = true
			if p.Candidate.ExpectedRemote != "" {
				snrefs[p.Candidate.ExpectedRemote] = true
			}
			mark(p.Candidate.Visible)
			mark(p.AcceptedLocal)
			return nil
		}); er != nil {
			return er
		}
		// A legacy accepted/pending snapshot may predate DocMedia. Its
		// immutable cache-bearing sibling must live as long as that revision.
		liveRevisions := map[string]bool{}
		for id := range snrefs {
			if r, ok := all[id]; ok {
				liveRevisions[r.Snapshot.Token+"\x00"+r.Snapshot.Revision] = true
			}
		}
		for id, r := range all {
			if len(r.Snapshot.DocMedia) > 0 && liveRevisions[r.Snapshot.Token+"\x00"+r.Snapshot.Revision] {
				snrefs[id] = true
			}
		}
		for id, r := range all {
			if snrefs[id] {
				sn := r.Snapshot
				for _, blob := range sn.Blobs() {
					mark(blob)
				}
			} else {
				if er := tx.Bucket([]byte("snapshots")).Delete([]byte(id)); er != nil {
					return er
				}
			}
		}
		return nil
	})
	if err != nil {
		return
	}
	files, err := os.ReadDir(filepath.Join(s.dir, "blobs"))
	if err != nil {
		return result, err
	}
	for _, f := range files {
		if err = ctx.Err(); err != nil {
			return
		}
		info, er := f.Info()
		if er != nil {
			return result, er
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if refs[model.BlobID(f.Name())] || o.Now.Sub(info.ModTime()) < PreparedBlobGrace {
			result.RetainedBytes += info.Size()
			continue
		}
		if er = os.Remove(filepath.Join(s.dir, "blobs", f.Name())); er != nil {
			return result, er
		}
		result.RemovedBlobs++
		result.RemovedBytes += info.Size()
	}
	if result.RemovedBlobs > 0 {
		err = s.io.SyncDir(filepath.Join(s.dir, "blobs"))
	}
	result.OverBudget = result.RetainedBytes > o.CleanBudgetBytes
	return
}
