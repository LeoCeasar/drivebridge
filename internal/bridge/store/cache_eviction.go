package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// CacheCandidate describes reconstructible clean content. Bytes includes the
// unique blobs released by this eviction; shared durable references still win.
type CacheCandidate struct {
	EntryID    model.EntryID
	Remote     string
	Generation uint64
	AccessedAt time.Time
	Bytes      int64
}

// TouchBodyAccess records successful use without allowing an older concurrent
// read to move the persisted LRU clock backwards.
func (s *Store) TouchBodyAccess(ctx context.Context, id model.EntryID, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		var m Metadata
		if err := get(tx, "metadata", string(id), &m); err != nil {
			return err
		}
		if !at.After(m.LastBodyAccess) {
			return nil
		}
		m.LastBodyAccess = at.UTC()
		return put(tx, "metadata", string(id), m)
	})
}

type cacheGraph struct {
	historySnapshots []model.SnapshotID
	entries          []model.Entry
	jobs             []jobRecord
	snapshots        []snapshotRecord
	namespaces       []namespaceRecord
	creates          []model.CreateOp
	conflicts        []model.Conflict
	recoveries       []recoveryRecord
	publications     []publication
}

func readCacheGraph(tx *bolt.Tx) (g cacheGraph, err error) {
	for _, f := range []func() error{
		func() error {
			return each[model.Entry](tx, "entries", func(v model.Entry) error {
				g.entries = append(g.entries, v)
				var m Metadata
				if err := get(tx, "metadata", string(v.ID), &m); err != nil {
					return err
				}
				if m.HistorySnapshot != "" && m.HistoryMediaBody == v.Local {
					g.historySnapshots = append(g.historySnapshots, m.HistorySnapshot)
				}
				return nil
			})
		},
		func() error {
			return each[jobRecord](tx, "jobs", func(v jobRecord) error { g.jobs = append(g.jobs, v); return nil })
		},
		func() error {
			return each[snapshotRecord](tx, "snapshots", func(v snapshotRecord) error { g.snapshots = append(g.snapshots, v); return nil })
		},
		func() error {
			return each[namespaceRecord](tx, "namespace", func(v namespaceRecord) error { g.namespaces = append(g.namespaces, v); return nil })
		},
		func() error {
			return each[model.CreateOp](tx, "createops", func(v model.CreateOp) error { g.creates = append(g.creates, v); return nil })
		},
		func() error {
			return each[model.Conflict](tx, "conflicts", func(v model.Conflict) error { g.conflicts = append(g.conflicts, v); return nil })
		},
		func() error {
			return each[recoveryRecord](tx, "recovery", func(v recoveryRecord) error { g.recoveries = append(g.recoveries, v); return nil })
		},
		func() error {
			return each[publication](tx, "publications", func(v publication) error { g.publications = append(g.publications, v); return nil })
		},
	} {
		if err = f(); err != nil {
			return
		}
	}
	return
}

type cachePrune struct {
	jobs       map[model.JobID]bool
	namespaces map[string]bool
	creates    map[string]bool
	snapshots  map[model.SnapshotID]bool
	blobs      map[model.BlobID]bool
}

func cleanBody(e model.Entry, m Metadata) bool {
	return e.Kind != model.KindFolder && e.Token != "" && e.Status == model.StatusSynced && e.Local != "" && e.Local == e.LocalCheckpoint && e.Local == e.Visible && e.RemoteCheckpoint != "" && e.Desired == "" && e.FrozenBy == "" && !m.Tombstone && m.VirtualType == "" && m.SafeSavePending == "" && !m.SafeSaveParked && m.SafeSaveOriginal == ""
}
func namespaceTouches(r namespaceRecord, e model.Entry) bool {
	if r.Op.SrcID == e.ID || r.Op.DstID == e.ID || r.Op.ResultID == e.ID {
		return true
	}
	for _, old := range r.Before {
		if old.ID == e.ID {
			return true
		}
	}
	return r.Op.Src != "" && overlaps(r.Op.Src, e.Remote) || r.Op.Dst != "" && overlaps(r.Op.Dst, e.Remote)
}
func namespaceOnlyEntry(r namespaceRecord, id model.EntryID) bool {
	for _, other := range []model.EntryID{r.Op.SrcID, r.Op.DstID, r.Op.ResultID} {
		if other != "" && other != id {
			return false
		}
	}
	for _, old := range r.Before {
		if old.ID != id {
			return false
		}
	}
	return true
}

// references computes every schema-owned blob and snapshot edge. Snapshot
// bodies are optional so unreferenced target history can be pruned first.
func (g cacheGraph) references(id model.EntryID, p cachePrune, includeSnapshots bool) (map[model.BlobID]bool, map[model.SnapshotID]bool) {
	bs := map[model.BlobID]bool{}
	ss := map[model.SnapshotID]bool{}
	b := func(v model.BlobID) {
		if v != "" {
			bs[v] = true
		}
	}
	sn := func(v model.SnapshotID) {
		if v != "" {
			ss[v] = true
		}
	}
	en := func(e model.Entry) {
		b(e.Local)
		b(e.Visible)
		b(e.LocalCheckpoint)
		b(e.Desired)
		sn(e.RemoteCheckpoint)
	}
	for _, id := range g.historySnapshots {
		sn(id)
	}
	for _, e := range g.entries {
		if e.ID != id {
			en(e)
		}
	}
	for _, j := range g.jobs {
		if !p.jobs[j.Job.ID] {
			b(j.Job.Local)
			b(j.Job.BaseLocal)
			b(j.Job.Plan)
			b(j.Desired)
			sn(j.Job.BaseRemote)
		}
	}
	for _, r := range g.namespaces {
		if !p.namespaces[r.Op.ID] {
			b(r.Op.Input)
			b(r.Op.Previous)
			for _, e := range r.Before {
				en(e)
			}
		}
	}
	for _, r := range g.creates {
		if !p.creates[r.ID] {
			b(r.Input)
		}
	}
	for _, c := range g.conflicts {
		b(c.Base)
		b(c.Local)
		b(c.Remote)
		b(c.Draft)
	}
	for _, r := range g.recoveries {
		sn(r.SnapshotID)
		for _, e := range r.Entries {
			en(e)
		}
	}
	for _, r := range g.publications {
		sn(r.Candidate.ExpectedRemote)
		sn(r.Candidate.NewRemote)
		b(r.Candidate.Visible)
		b(r.AcceptedLocal)
	}
	if includeSnapshots {
		for _, r := range g.snapshots {
			if !p.snapshots[r.Snapshot.ID] {
				for _, blob := range r.Snapshot.Blobs() {
					b(blob)
				}
			}
		}
	}
	return bs, ss
}
func (g cacheGraph) evictionPlan(e model.Entry, m Metadata, leases map[model.BlobID]int) (cachePrune, bool) {
	p := cachePrune{jobs: map[model.JobID]bool{}, namespaces: map[string]bool{}, creates: map[string]bool{}, snapshots: map[model.SnapshotID]bool{}, blobs: map[model.BlobID]bool{}}
	if !cleanBody(e, m) {
		return p, false
	}
	protectedJobs := map[model.JobID]bool{}
	protectedNS := map[string]bool{}
	protectedSN := map[model.SnapshotID]bool{}
	for _, r := range g.recoveries {
		protectedJobs[r.JobID] = true
		protectedNS[r.NamespaceID] = true
		protectedSN[r.SnapshotID] = true
		for _, old := range r.Entries {
			if old.ID == e.ID {
				return p, false
			}
		}
	}
	for _, r := range g.snapshots {
		if r.Snapshot.Token == e.Token && protectedSN[r.Snapshot.ID] {
			return p, false
		}
	}
	for _, c := range g.conflicts {
		if c.EntryID == e.ID {
			return p, false
		}
	}
	for _, r := range g.publications {
		if r.Candidate.EntryID == e.ID {
			return p, false
		}
	}
	// Repeated autosaves can leave proven unsent predecessors. They are safe
	// to prune only after the current generation has its own cloud confirmation;
	// the clean entry flags alone are not evidence for discarding older saves.
	currentConfirmed := false
	for _, j := range g.jobs {
		if j.Job.EntryID == e.ID && j.Job.Generation == e.Generation && j.Job.State == model.StatusSynced && !j.CompletedAt.IsZero() && !j.Superseded && !j.LocalReceipt {
			currentConfirmed = true
		}
	}
	createJobs := map[model.JobID]bool{}
	for _, r := range g.creates {
		createJobs[r.JobID] = true
	}
	for _, j := range g.jobs {
		if j.Job.EntryID == e.ID {
			confirmed := j.Job.State == model.StatusSynced && !j.CompletedAt.IsZero()
			overwrittenUnsent := currentConfirmed && j.Superseded && j.Job.Generation < e.Generation && unsentJob(j) && !createJobs[j.Job.ID]
			if (!confirmed && !overwrittenUnsent) || j.LocalReceipt || protectedJobs[j.Job.ID] {
				return p, false
			}
			p.jobs[j.Job.ID] = true
		}
	}
	for _, r := range g.creates {
		if r.EntryID == e.ID {
			if r.State != "finished" {
				return p, false
			}
			p.creates[r.ID] = true
		}
	}
	for _, r := range g.namespaces {
		if namespaceTouches(r, e) {
			if r.Op.State != "finished" || r.ActionPending || r.Dispatched || r.LocalReceipt || r.LocalCancel || r.SafeSaveBackup || r.SafeSaveRestore || protectedNS[r.Op.ID] || !namespaceOnlyEntry(r, e.ID) {
				return p, false
			}
			for _, other := range g.namespaces {
				if other.WaitFor == r.Op.ID {
					return p, false
				}
			}
			p.namespaces[r.Op.ID] = true
		}
	}
	// Only the target token's history is reconstructible here. References from
	// another entry, pending work, conflict or recovery retain shared snapshots.
	_, snrefs := g.references(e.ID, p, false)
	// A second path can still reference a pre-upgrade baseline whose media was
	// filled by a sibling snapshot. Evicting one path must retain that bundle.
	liveRevisions := map[string]bool{}
	for _, r := range g.snapshots {
		if snrefs[r.Snapshot.ID] {
			liveRevisions[r.Snapshot.Token+"\x00"+r.Snapshot.Revision] = true
		}
	}
	for _, r := range g.snapshots {
		if len(r.Snapshot.DocMedia) > 0 && liveRevisions[r.Snapshot.Token+"\x00"+r.Snapshot.Revision] {
			snrefs[r.Snapshot.ID] = true
		}
	}
	for _, r := range g.snapshots {
		if r.Snapshot.Token == e.Token && !snrefs[r.Snapshot.ID] {
			p.snapshots[r.Snapshot.ID] = true
		}
	}
	// Diff the complete graph rather than guessing which historical blobs belong
	// to the entry. This also protects shared blobs and snapshot sidecars.
	before, _ := g.references("", cachePrune{}, true)
	after, _ := g.references(e.ID, p, true)
	for b := range before {
		if !after[b] {
			p.blobs[b] = true
		}
	}
	// An active reader prevents changing the placeholder as well as unlinking.
	if leases[e.Local] > 0 {
		return p, false
	}
	for b := range p.blobs {
		if leases[b] > 0 {
			return p, false
		}
	}
	return p, true
}

func (s *Store) CleanCacheCandidates(ctx context.Context) (out []CacheCandidate, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		g, er := readCacheGraph(tx)
		if er != nil {
			return er
		}
		now := time.Now().UTC()
		for _, e := range g.entries {
			if er = ctx.Err(); er != nil {
				return er
			}
			var m Metadata
			if er = get(tx, "metadata", string(e.ID), &m); er != nil {
				return er
			}
			p, ok := g.evictionPlan(e, m, s.leases)
			if !ok {
				continue
			}
			if m.LastBodyAccess.IsZero() {
				m.LastBodyAccess = now
				if er = put(tx, "metadata", string(e.ID), m); er != nil {
					return er
				}
			}
			c := CacheCandidate{EntryID: e.ID, Remote: e.Remote, Generation: e.Generation, AccessedAt: m.LastBodyAccess}
			for b := range p.blobs {
				n, er := s.blobSize(b)
				if er != nil {
					return er
				}
				c.Bytes += n
			}
			out = append(out, c)
		}
		return nil
	})
	return
}

// EvictCleanEntry must be surrounded by the caller's path reservation and open
// file/dirty-cache check. All durable state and blob leases are rechecked here.
// The placeholder identity and generation remain stable for re-materialization.
func (s *Store) EvictCleanEntry(ctx context.Context, id model.EntryID, expectedGeneration uint64, expectedAccess ...time.Time) (evicted bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return
	}
	if !s.bound {
		return false, ErrUnbound
	}
	if s.closed {
		return false, bolt.ErrDatabaseNotOpen
	}
	var blobs map[model.BlobID]bool
	err = s.db.Update(func(tx *bolt.Tx) error {
		e, er := entryTx(tx, id)
		if er != nil {
			return er
		}
		if e.Generation != expectedGeneration {
			return nil
		}
		var m Metadata
		if er = get(tx, "metadata", string(id), &m); er != nil {
			return er
		}
		if len(expectedAccess) > 0 && !m.LastBodyAccess.Equal(expectedAccess[0]) {
			return nil
		}
		g, er := readCacheGraph(tx)
		if er != nil {
			return er
		}
		p, ok := g.evictionPlan(e, m, s.leases)
		if !ok {
			return nil
		}
		for _, j := range g.jobs {
			if p.jobs[j.Job.ID] {
				if er = tx.Bucket([]byte("jobs")).Delete([]byte(j.Job.ID)); er != nil {
					return er
				}
			}
		}
		for _, r := range g.creates {
			if p.creates[r.ID] {
				if er = tx.Bucket([]byte("createops")).Delete([]byte(r.ID)); er != nil {
					return er
				}
			}
		}
		for _, r := range g.namespaces {
			if p.namespaces[r.Op.ID] {
				if er = tx.Bucket([]byte("namespace")).Delete([]byte(r.Op.ID)); er != nil {
					return er
				}
			}
		}
		for _, r := range g.snapshots {
			if p.snapshots[r.Snapshot.ID] {
				if er = tx.Bucket([]byte("snapshots")).Delete([]byte(r.Snapshot.ID)); er != nil {
					return er
				}
			}
		}
		e.Local = ""
		e.Visible = ""
		e.LocalCheckpoint = ""
		e.RemoteCheckpoint = ""
		e.Desired = ""
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		if er = ctx.Err(); er != nil {
			return er
		}
		if er = historyProtectedTx(tx, func(b model.BlobID) { delete(p.blobs, b) }); er != nil {
			return er
		}
		blobs = p.blobs
		evicted = true
		return nil
	})
	if err != nil || !evicted {
		return
	}
	close(s.changed)
	s.changed = make(chan struct{})
	// Only blobs released by this transaction are eligible; new preparations keep
	// the same grace as ordinary GC. Failure leaves harmless unreferenced bytes.
	now := time.Now().UTC()
	removed := false
	for b := range blobs {
		if err = ctx.Err(); err != nil {
			return
		}
		info, er := os.Stat(s.blobPath(b))
		if errors.Is(er, os.ErrNotExist) {
			continue
		}
		if er != nil {
			return true, er
		}
		if s.leases[b] > 0 || now.Sub(info.ModTime()) < PreparedBlobGrace {
			continue
		}
		if er = os.Remove(s.blobPath(b)); er != nil {
			return true, er
		}
		removed = true
	}
	if removed {
		err = s.io.SyncDir(filepath.Join(s.dir, "blobs"))
	}
	return
}
