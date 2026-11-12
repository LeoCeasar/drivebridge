package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

type jobRecord struct {
	LocalReceipt       bool                    `json:"local_receipt,omitempty"`
	StagedAt           time.Time               `json:"staged_at,omitempty"`
	PreparationRestart bool                    `json:"preparation_restart,omitempty"`
	HumanResolution    bool                    `json:"human_resolution,omitempty"`
	ErrorCode          string                  `json:"error_code,omitempty"`
	ResumeState        model.Status            `json:"resume_state,omitempty"`
	Superseded         bool                    `json:"superseded,omitempty"`
	Job                model.Job               `json:"job"`
	Operations         []model.Operation       `json:"operations,omitempty"`
	Results            map[int]json.RawMessage `json:"results,omitempty"`
	Desired            model.BlobID            `json:"desired,omitempty"`
	CompletedAt        time.Time               `json:"completed_at,omitempty"`
}

func jobTx(tx *bolt.Tx, id model.JobID) (j jobRecord, err error) {
	err = get(tx, "jobs", string(id), &j)
	return
}
func saveJob(tx *bolt.Tx, j jobRecord) error { return put(tx, "jobs", string(j.Job.ID), j) }
func active(st model.Status) bool {
	return st == model.StatusMerging || st == model.StatusUploading || st == model.StatusAwaitingVerification
}
func pending(st model.Status) bool {
	return st != model.StatusSynced && st != model.StatusAwaitingPublish
}
func hasPending(tx *bolt.Tx, id model.EntryID) bool {
	yes := false
	_ = each[jobRecord](tx, "jobs", func(j jobRecord) error {
		if j.Job.EntryID == id && !j.Superseded && pending(j.Job.State) {
			yes = true
		}
		return nil
	})
	return yes
}
func inflight(tx *bolt.Tx, id model.EntryID, except model.JobID) bool {
	yes := false
	_ = each[jobRecord](tx, "jobs", func(j jobRecord) error {
		if j.Job.EntryID == id && j.Job.ID != except && active(j.Job.State) {
			yes = true
		}
		return nil
	})
	return yes
}
func (s *Store) stageBlob(tx *bolt.Tx, id model.EntryID, expected *uint64, b model.BlobID) (e model.Entry, j model.Job, err error) {
	e, err = entryTx(tx, id)
	if err != nil {
		return
	}
	if expected != nil && e.Generation != *expected {
		return e, j, model.ErrGenerationChanged
	}
	var m Metadata
	if err = get(tx, "metadata", string(id), &m); err != nil {
		return
	}
	if m.Tombstone {
		return e, j, ErrNotFound
	}
	if e.Kind == model.KindFolder {
		return e, j, errors.New("cannot stage folder content")
	}
	if e.FrozenBy != "" {
		var op namespaceRecord
		if err = get(tx, "namespace", e.FrozenBy, &op); err != nil {
			return
		}
		if op.Op.State != "prepared" {
			return e, j, ErrFrozen
		}
	}
	// Only provably undispatched predecessors can be superseded by a newer save.
	if err = supersedeUnsentBefore(tx, e.ID, e.Generation+1); err != nil {
		return e, j, err
	}
	e.Generation++
	e.Local = b
	e.Visible = b
	e.Status = model.StatusQueued
	j = model.Job{ID: model.JobID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Local: b, BaseLocal: e.LocalCheckpoint, BaseRemote: e.RemoteCheckpoint, State: model.StatusQueued}
	if err = saveEntry(tx, e); err != nil {
		return
	}
	err = saveJob(tx, jobRecord{Job: j, StagedAt: time.Now().UTC()})
	if err == nil {
		err = completeSafeSave(tx, e)
	}
	return
}

// unsentJob checks journal-local evidence; callers must separately exclude any
// create journal before treating it as proof that no mutation was dispatched.
func unsentJob(r jobRecord) bool {
	safeState := r.Job.State == model.StatusQueued || (r.Job.State == model.StatusBlocked || r.Job.State == model.StatusPaused) && r.ResumeState == model.StatusMerging
	return safeState && r.Job.Plan == "" && len(r.Job.OperationIDs) == 0 && len(r.Operations) == 0 && len(r.Results) == 0 && r.Desired == ""
}

// liveJobEntry is checked in the same write transaction that claims dispatch.
// A previously read job cannot be revived after local cancellation.
func liveJobEntry(tx *bolt.Tx, r jobRecord) (model.Entry, error) {
	if r.Superseded || r.LocalReceipt {
		return model.Entry{}, ErrNotFound
	}
	e, err := entryTx(tx, r.Job.EntryID)
	if err != nil {
		return e, err
	}
	var metadata Metadata
	if err = get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return e, err
	}
	if metadata.Tombstone {
		return e, ErrNotFound
	}
	return e, nil
}

// supersedeUnsentBefore retains every journal and blob. A plan, operation,
// result, create journal, or unknown/active state forbids cancellation.
func supersedeUnsentBefore(tx *bolt.Tx, id model.EntryID, generation uint64) error {
	creates := map[model.JobID]bool{}
	if err := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error { creates[op.JobID] = true; return nil }); err != nil {
		return err
	}
	return each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID != id || r.Job.Generation >= generation || r.Superseded || !unsentJob(r) || creates[r.Job.ID] {
			return nil
		}
		r.Superseded = true
		return saveJob(tx, r)
	})
}

// SupersedeUnsentPredecessors recovers obsolete pre-dispatch work, including
// queues saved by older versions. It never labels that historical work synced.
func (s *Store) SupersedeUnsentPredecessors(ctx context.Context, id model.EntryID) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		e, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		if e.FrozenBy != "" {
			return ErrFrozen
		}
		return supersedeUnsentBefore(tx, id, e.Generation)
	})
}

func (s *Store) Stage(ctx context.Context, id model.EntryID, expected *uint64, src io.Reader) (model.Entry, model.Job, error) {
	return s.stage(ctx, id, expected, src, false)
}

// StageLocalOnly accepts durable local bytes without claiming cloud delivery.
// Ambiguous history falls back to the ordinary outbox and remains visible.
func (s *Store) StageLocalOnly(ctx context.Context, id model.EntryID, expected *uint64, src io.Reader) (model.Entry, model.Job, error) {
	return s.stage(ctx, id, expected, src, true)
}

func (s *Store) stage(ctx context.Context, id model.EntryID, expected *uint64, src io.Reader, localOnly bool) (e model.Entry, j model.Job, err error) {
	current, err := s.Entry(ctx, id)
	if err != nil {
		return e, j, err
	}
	if current.Kind == model.KindDocx {
		src = io.LimitReader(src, LargeSnapshotBytes+1)
	}
	b, err := s.PutBlob(ctx, src)
	if err != nil {
		return e, j, err
	}
	return s.StagePreparedBlob(ctx, id, expected, b, localOnly)
}

// StagePreparedBlob accepts bytes already persisted by PutBlob.
func (s *Store) StagePreparedBlob(ctx context.Context, id model.EntryID, expected *uint64, b model.BlobID, localOnly bool) (e model.Entry, j model.Job, err error) {
	if !blobName.MatchString(string(b)) {
		return e, j, errors.New("invalid blob ID")
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		before, er := entryTx(tx, id)
		if er != nil {
			return er
		}
		size, er := s.blobSize(b)
		if er != nil {
			return er
		}
		if before.Kind == model.KindDocx && size > LargeSnapshotBytes {
			return model.ErrUnsupportedContent
		}
		receipt := false
		if localOnly {
			receipt, er = eligibleLocalReceipt(tx, before, "")
			if er != nil {
				return er
			}
		}
		e, j, err = s.stageBlob(tx, id, expected, b)
		if err != nil {
			return err
		}
		if receipt {
			return recordLocalReceipt(tx, j.ID)
		}
		return nil
	})
	return
}
func (s *Store) Prepare(ctx context.Context, id model.JobID) (model.Job, model.Entry, error) {
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return model.Job{}, model.Entry{}, err
		}
		if !s.bound {
			s.mu.Unlock()
			return model.Job{}, model.Entry{}, ErrUnbound
		}
		if s.closed {
			s.mu.Unlock()
			return model.Job{}, model.Entry{}, bolt.ErrDatabaseNotOpen
		}
		var rec jobRecord
		var e model.Entry
		err := s.db.Update(func(tx *bolt.Tx) error {
			var err error
			rec, err = jobTx(tx, id)
			if err != nil {
				return err
			}
			e, err = entryTx(tx, rec.Job.EntryID)
			if err != nil {
				return err
			}
			if e.FrozenBy != "" {
				return ErrFrozen
			}
			if rec.Job.State == model.StatusAwaitingVerification {
				return model.ErrUnknownResult
			}
			if rec.Superseded {
				return ErrNotFound
			}
			if rec.Job.State != model.StatusQueued {
				return ErrInvalidState
			}
			if inflight(tx, e.ID, id) || priorPending(tx, rec.Job) {
				return ErrBusy
			}
			var m Metadata
			if err = get(tx, "metadata", string(e.ID), &m); err != nil {
				return err
			}
			if m.Tombstone {
				return ErrNotFound
			}
			rec.Job.BaseLocal = e.LocalCheckpoint
			rec.Job.BaseRemote = e.RemoteCheckpoint
			rec.Job.State = model.StatusMerging
			if rec.Job.Generation == e.Generation {
				e.Status = rec.Job.State
				if err = saveEntry(tx, e); err != nil {
					return err
				}
			}
			return saveJob(tx, rec)
		})
		ch := s.changed
		if err == nil {
			close(s.changed)
			s.changed = make(chan struct{})
		}
		s.mu.Unlock()
		if !errors.Is(err, ErrBusy) {
			return rec.Job, e, err
		}
		select {
		case <-ctx.Done():
			return model.Job{}, model.Entry{}, ctx.Err()
		case <-ch:
		}
	}
}
func (s *Store) RecordPlan(ctx context.Context, id model.JobID, ops []model.Operation, desired model.BlobID) error {
	if err := s.checkBlob(desired); err != nil {
		return err
	}
	ops = append([]model.Operation(nil), ops...)
	for i := range ops {
		if ops[i].ID == "" {
			ops[i].ID = uuid.NewString()
		}
	}
	encoded, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	b, err := s.PutBlob(ctx, strings.NewReader(string(encoded)))
	if err != nil {
		return err
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		e, err := liveJobEntry(tx, r)
		if err != nil {
			return err
		}
		if r.Job.State != model.StatusMerging && !(r.Job.State == model.StatusAwaitingVerification && r.PreparationRestart && r.Job.Plan == "") {
			return ErrInvalidState
		}
		r.PreparationRestart = false
		r.Operations = ops
		r.Job.OperationIDs = nil
		for _, op := range ops {
			r.Job.OperationIDs = append(r.Job.OperationIDs, op.ID)
		}
		r.Job.Plan = b
		r.Desired = desired
		r.Job.State = model.StatusUploading
		if e.Generation == r.Job.Generation {
			e.Desired = desired
			e.Status = model.StatusUploading
			if err = saveEntry(tx, e); err != nil {
				return err
			}
		}
		return saveJob(tx, r)
	})
}
func (s *Store) RecordOperation(ctx context.Context, id model.JobID, index int, result json.RawMessage) error {
	if !json.Valid(result) {
		return errors.New("invalid operation result")
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		if r.Job.State != model.StatusUploading && r.Job.State != model.StatusAwaitingVerification {
			return ErrInvalidState
		}
		if index < 0 || index >= len(r.Operations) {
			return errors.New("invalid operation index")
		}
		if r.Results == nil {
			r.Results = make(map[int]json.RawMessage)
		}
		if old, ok := r.Results[index]; ok && string(old) != string(result) {
			return model.ErrUnknownResult
		}
		r.Results[index] = append(json.RawMessage(nil), result...)
		return saveJob(tx, r)
	})
}
func (s *Store) MarkUnknown(ctx context.Context, id model.JobID) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		if !active(r.Job.State) {
			return ErrInvalidState
		}
		r.Job.State = model.StatusAwaitingVerification
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
func (s *Store) Job(ctx context.Context, id model.JobID) (j model.Job, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error { r, er := jobTx(tx, id); j = r.Job; return er })
	return
}

type JobJournal struct {
	LocalReceipt       bool
	StagedAt           time.Time
	PreparationRestart bool
	HumanResolution    bool
	ErrorCode          string

	Job        model.Job
	Operations []model.Operation
	Results    map[int]json.RawMessage
	Desired    model.BlobID
}

func (s *Store) Journal(ctx context.Context, id model.JobID) (j JobJournal, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		r, er := jobTx(tx, id)
		j = JobJournal{LocalReceipt: r.LocalReceipt, StagedAt: r.StagedAt, PreparationRestart: r.PreparationRestart, HumanResolution: r.HumanResolution, ErrorCode: r.ErrorCode, Job: r.Job, Operations: r.Operations, Results: r.Results, Desired: r.Desired}
		return er
	})
	return
}
func (s *Store) Pending(ctx context.Context) (out []model.Job, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[jobRecord](tx, "jobs", func(r jobRecord) error {
			if !r.Superseded && pending(r.Job.State) {
				out = append(out, r.Job)
			}
			return nil
		})
	})
	if err == nil {
		sort.Slice(out, func(i, j int) bool {
			if out[i].EntryID == out[j].EntryID {
				return out[i].Generation < out[j].Generation
			}
			return out[i].EntryID < out[j].EntryID
		})
	}
	return
}
func (s *Store) Confirm(ctx context.Context, id model.JobID, actual model.Snapshot) (c model.PublishCandidate, err error) {
	actual, err = s.prepareSnapshot(actual)
	if err != nil {
		return
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		r, er := jobTx(tx, id)
		if er != nil {
			return er
		}
		e, er := entryTx(tx, r.Job.EntryID)
		if er != nil {
			return er
		}
		if e.Token == "" || actual.Token != e.Token {
			return model.ErrIdentityChanged
		}
		if (e.Kind == model.KindFile && actual.FileData == "") || (e.Kind == model.KindDocx && actual.FileData != "") {
			return model.ErrUnsupportedContent
		}
		if !active(r.Job.State) {
			return ErrInvalidState
		}
		if er = s.saveSnapshot(tx, actual); er != nil {
			return er
		}
		e.LocalCheckpoint = r.Job.Local
		e.RemoteCheckpoint = actual.ID
		r.Job.State = model.StatusAwaitingPublish
		r.CompletedAt = time.Now().UTC()
		if e.Generation == r.Job.Generation {
			e.Status = model.StatusAwaitingPublish
		}
		if er = saveJob(tx, r); er != nil {
			return er
		}
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		c = model.PublishCandidate{EntryID: e.ID, ExpectedGeneration: r.Job.Generation, ExpectedRemote: actual.ID, NewRemote: actual.ID, Visible: snapshotContent(actual)}
		return saveCandidate(tx, c, r.Job.Local)
	})
	return
}

type publication struct {
	Candidate     model.PublishCandidate `json:"candidate"`
	AcceptedLocal model.BlobID           `json:"accepted_local"`
}

func candidateKey(c model.PublishCandidate) string {
	return string(c.EntryID) + ":" + string(c.NewRemote)
}
func saveCandidate(tx *bolt.Tx, c model.PublishCandidate, local model.BlobID) error {
	return put(tx, "publications", candidateKey(c), publication{c, local})
}
func (s *Store) OfferRemote(ctx context.Context, id model.EntryID, generation uint64, actual model.Snapshot, visible model.BlobID) (c model.PublishCandidate, err error) {
	actual, err = s.prepareSnapshot(actual)
	if err != nil {
		return
	}
	if visible == "" {
		visible = snapshotContent(actual)
	}
	if err = s.checkBlob(visible); err != nil {
		return
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		e, er := entryTx(tx, id)
		if er != nil {
			return er
		}
		if e.Generation != generation {
			return model.ErrGenerationChanged
		}
		if e.Token == "" || e.Token != actual.Token {
			return model.ErrIdentityChanged
		}
		if (e.Kind == model.KindFile && (actual.FileData == "" || actual.FileData != visible)) || (e.Kind == model.KindDocx && actual.FileData != "") {
			return model.ErrUnsupportedContent
		}
		if e.FrozenBy != "" {
			return ErrFrozen
		}
		if hasPending(tx, id) || e.Local != e.LocalCheckpoint {
			return ErrBusy
		}
		if er = s.saveSnapshot(tx, actual); er != nil {
			return er
		}
		if er = s.checkBlob(visible); er != nil {
			return er
		}
		c = model.PublishCandidate{EntryID: id, ExpectedGeneration: generation, ExpectedRemote: e.RemoteCheckpoint, NewRemote: actual.ID, Visible: visible}
		return saveCandidate(tx, c, "")
	})
	return
}
func (s *Store) CommitPublication(ctx context.Context, c model.PublishCandidate) (e model.Entry, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		e, er = entryTx(tx, c.EntryID)
		if er != nil {
			return er
		}
		if e.Generation != c.ExpectedGeneration {
			return model.ErrGenerationChanged
		}
		if e.RemoteCheckpoint != c.ExpectedRemote {
			return model.ErrVersionChanged
		}
		if e.FrozenBy != "" {
			return ErrFrozen
		}
		var p publication
		if er = get(tx, "publications", candidateKey(c), &p); er != nil {
			return er
		}
		if p.Candidate != c {
			return ErrInvalidState
		}
		if hasPending(tx, e.ID) {
			return ErrBusy
		}
		var sn snapshotRecord
		if er = get(tx, "snapshots", string(c.NewRemote), &sn); er != nil {
			return er
		}
		if e.Token != sn.Snapshot.Token {
			return model.ErrIdentityChanged
		}
		e.RemoteCheckpoint = c.NewRemote
		e.Local = c.Visible
		e.Visible = c.Visible
		e.LocalCheckpoint = c.Visible
		e.Desired = ""
		e.Status = model.StatusSynced
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		if er = each[jobRecord](tx, "jobs", func(j jobRecord) error {
			if j.Job.EntryID == e.ID && j.Job.Generation == c.ExpectedGeneration && j.Job.State == model.StatusAwaitingPublish {
				j.Job.State = model.StatusSynced
				return saveJob(tx, j)
			}
			return nil
		}); er != nil {
			return er
		}
		return tx.Bucket([]byte("publications")).Delete([]byte(candidateKey(c)))
	})
	return
}

// A process may have exited after dispatch but before journaling a result. Reopen
// therefore requires observation; it never converts interrupted work to queued.
func recoverInterrupted(tx *bolt.Tx) error {
	if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if active(r.Job.State) {
			if r.Job.State == model.StatusMerging && r.Job.Plan == "" {
				hasCreate := false
				if err := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
					if op.JobID == r.Job.ID && op.State != "finished" && op.State != "not_dispatched" {
						hasCreate = true
					}
					return nil
				}); err != nil {
					return err
				}
				r.PreparationRestart = !hasCreate
			}
			r.Job.State = model.StatusAwaitingVerification
			if err := saveJob(tx, r); err != nil {
				return err
			}
			e, err := entryTx(tx, r.Job.EntryID)
			if err != nil {
				return err
			}
			if e.Generation == r.Job.Generation {
				e.Status = model.StatusAwaitingVerification
				return saveEntry(tx, e)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
		if op.State == "prepared" {
			op.State = "unknown"
			return put(tx, "createops", op.ID, op)
		}
		return nil
	})
}

func priorPending(tx *bolt.Tx, job model.Job) bool {
	yes := false
	_ = each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID == job.EntryID && r.Job.Generation < job.Generation && !r.Superseded && pending(r.Job.State) {
			yes = true
		}
		return nil
	})
	return yes
}

// PendingPublications resumes local publication after a durable cloud confirmation.
func (s *Store) PendingPublications(ctx context.Context) (out []model.PublishCandidate, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[publication](tx, "publications", func(p publication) error {
			e, er := entryTx(tx, p.Candidate.EntryID)
			if er != nil {
				return er
			}
			if e.Generation == p.Candidate.ExpectedGeneration && e.RemoteCheckpoint == p.Candidate.ExpectedRemote {
				out = append(out, p.Candidate)
			}
			return nil
		})
	})
	return
}
