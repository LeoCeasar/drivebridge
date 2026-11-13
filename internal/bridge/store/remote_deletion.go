package store

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// deletionJobUndispatched is deliberately narrower than merely non-active.
// Merging means only a read/merge preparation when every mutation journal is empty.
func deletionJobUndispatched(r jobRecord) bool {
	switch r.Job.State {
	case model.StatusQueued, model.StatusConflict, model.StatusMerging:
	case model.StatusBlocked, model.StatusPaused:
		if r.ResumeState != "" && r.ResumeState != model.StatusMerging && r.ResumeState != model.StatusQueued && r.ResumeState != model.StatusConflict {
			return false
		}
	default:
		return false
	}
	return r.Job.Plan == "" && len(r.Job.OperationIDs) == 0 && len(r.Operations) == 0 && len(r.Results) == 0 && r.Desired == ""
}

// deletionWorkSafe never discards ambiguous work. Completed historical journals
// remain as recovery material; current publication and namespace work must finish.
func deletionWorkSafe(tx *bolt.Tx, e model.Entry) error {
	if e.FrozenBy != "" || namespaceReserved(tx, e.Remote) {
		return ErrFrozen
	}
	if e.Status == model.StatusAwaitingVerification {
		return model.ErrUnknownResult
	}
	if e.Desired != "" {
		return ErrBusy
	}
	if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID != e.ID {
			return nil
		}
		if r.Job.State == model.StatusAwaitingVerification {
			return model.ErrUnknownResult
		}
		if !r.Superseded && pending(r.Job.State) && !deletionJobUndispatched(r) {
			return ErrBusy
		}
		return nil
	}); err != nil {
		return err
	}
	if err := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
		if op.EntryID == e.ID && op.State != "finished" && op.State != "not_dispatched" {
			return model.ErrUnknownResult
		}
		return nil
	}); err != nil {
		return err
	}
	if err := each[publication](tx, "publications", func(p publication) error {
		if p.Candidate.EntryID == e.ID && p.Candidate.ExpectedGeneration == e.Generation && p.Candidate.ExpectedRemote == e.RemoteCheckpoint {
			return ErrBusy
		}
		return nil
	}); err != nil {
		return err
	}
	return each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
		reserved := r.Op.State == "prepared" || r.Op.State == "sealed" || r.Op.State == "committed"
		if (reserved || r.ActionPending || r.Dispatched) && (namespaceTouches(r, e) || (r.Op.Src != "" && overlaps(r.Op.Src, e.Remote)) || (r.Op.Dst != "" && overlaps(r.Op.Dst, e.Remote))) {
			if r.Dispatched {
				return model.ErrUnknownResult
			}
			return ErrFrozen
		}
		return nil
	})
}

func resolveEntryConflicts(tx *bolt.Tx, id model.EntryID) error {
	return each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
		if c.EntryID != id || c.Resolved {
			return nil
		}
		c.Resolved = true
		return put(tx, "conflicts", string(c.ID), c)
	})
}

func supersedeDeletionJobs(tx *bolt.Tx, id model.EntryID) error {
	return each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID != id || r.Superseded || !pending(r.Job.State) {
			return nil
		}
		if !deletionJobUndispatched(r) {
			return ErrBusy
		}
		r.Superseded = true
		return saveJob(tx, r)
	})
}

func protectDeletion(tx *bolt.Tx, e model.Entry) error {
	return put(tx, "recovery", fmt.Sprintf("remote-delete:%s:%d", e.ID, e.Generation), recoveryRecord{Entries: []model.Entry{e}})
}

// ObserveRemoteDeletion applies a complete, identity-checked parent listing's
// absence proof. The caller owns that proof; this transaction checks its captured
// entry against all local state that could have changed while listing the cloud.
// The bool is true only when this observation tombstones a clean entry.
func (s *Store) ObserveRemoteDeletion(ctx context.Context, expected model.Entry) (out model.Conflict, removed bool, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		if managedEntryPending(tx, expected.ID) {
			return ErrBusy
		}
		e, er := entryTx(tx, expected.ID)
		if er != nil {
			return er
		}
		var metadata Metadata
		if er = get(tx, "metadata", string(e.ID), &metadata); er != nil {
			return er
		}
		if metadata.Tombstone || e.Token == "" || (e.Kind != model.KindFile && e.Kind != model.KindDocx) || metadata.VirtualType != "" {
			return nil
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
		if metadata.FuseSoftRemove != "" || metadata.SafeSavePending != "" || metadata.SafeSaveParked || metadata.SafeSaveOriginal != "" {
			return ErrFrozen
		}
		localOnly := false
		if er = each[jobRecord](tx, "jobs", func(r jobRecord) error {
			if r.Job.EntryID == e.ID && r.Job.Generation == e.Generation && r.LocalReceipt {
				localOnly = true
			}
			return nil
		}); er != nil {
			return er
		}
		if localOnly {
			return nil
		}
		if er = deletionWorkSafe(tx, e); er != nil {
			return er
		}
		var conflicts []model.Conflict
		if er = each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if c.EntryID == e.ID && !c.Resolved {
				conflicts = append(conflicts, c)
			}
			return nil
		}); er != nil {
			return er
		}
		if e.Local == e.LocalCheckpoint && !hasPending(tx, e.ID) && len(conflicts) == 0 && e.Status != model.StatusConflict {
			if er = protectDeletion(tx, e); er != nil {
				return er
			}
			e.Generation++
			e.Status = model.StatusSynced
			// Remote absence is not a persistent local-delete intent. The
			// protected entry retains this binding; a later positive cloud
			// listing may admit the same token as a fresh local placeholder.
			e.Token = ""
			if er = tombstone(tx, e); er != nil {
				return er
			}
			removed = true
			return nil
		}
		out = model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Base: e.LocalCheckpoint, Local: e.Local}
		if len(conflicts) > 0 {
			// Bucket order provides a stable preferred ID even after every
			// duplicate has acquired the same remote-deletion reason.
			out = conflicts[0]
		}
		if len(conflicts) == 0 {
			conflicts = append(conflicts, out)
		}
		// Older versions could leave multiple unresolved records for one
		// entry. Every ID remains independently refreshable, with its own
		// draft and base preserved, and captures the same current local bytes.
		for _, c := range conflicts {
			c.Generation = e.Generation
			c.Local = e.Local
			if c.Base == "" {
				c.Base = e.LocalCheckpoint
			}
			c.Reason = model.ConflictRemoteDeleted
			c.RemoteRevision = model.DeletedRevision(e.Token)
			c.Remote = ""
			if er = put(tx, "conflicts", string(c.ID), c); er != nil {
				return er
			}
			if c.ID == out.ID {
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

// ResolveRemoteDeletion settles a captured cloud-deletion or unsent local
// unlink decision. Restore stages a tokenless creation at the original path.
func (s *Store) ResolveRemoteDeletion(ctx context.Context, r model.ResolveRequest) (e model.Entry, j model.Job, err error) {
	if r.Action != model.ResolveRestoreLocal && r.Action != model.ResolveAcceptDelete {
		return e, j, ErrInvalidState
	}
	var content model.BlobID
	if r.Action == model.ResolveRestoreLocal && r.Content != nil {
		content, err = s.PutBlob(ctx, bytes.NewReader(r.Content))
		if err != nil {
			return
		}
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if er := get(tx, "conflicts", string(r.ConflictID), &c); er != nil {
			return er
		}
		if c.Resolved || (c.Reason != model.ConflictRemoteDeleted && c.Reason != model.ConflictLocalDeletePending) {
			return ErrInvalidState
		}
		var er error
		e, er = entryTx(tx, c.EntryID)
		if er != nil {
			return er
		}
		if e.Generation != r.ExpectedGeneration || c.Generation != e.Generation {
			return model.ErrGenerationChanged
		}
		if c.RemoteRevision != r.ExpectedRemoteRevision {
			return model.ErrVersionChanged
		}
		localDeletion := c.Reason == model.ConflictLocalDeletePending
		if localDeletion {
			if e.Token != "" || e.LocalCheckpoint != "" || e.RemoteCheckpoint != "" || c.RemoteRevision != model.LocalDeletionRevision(e.ID, e.Generation) {
				return model.ErrVersionChanged
			}
		} else if e.Token == "" || c.RemoteRevision != model.DeletedRevision(e.Token) {
			return model.ErrVersionChanged
		}
		var metadata Metadata
		if er = get(tx, "metadata", string(e.ID), &metadata); er != nil {
			return er
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		if e.Kind != model.KindFile && e.Kind != model.KindDocx {
			return model.ErrUnsupportedContent
		}
		if metadata.VirtualType != "" || metadata.FuseSoftRemove != "" || metadata.SafeSavePending != "" || metadata.SafeSaveParked || metadata.SafeSaveOriginal != "" {
			return ErrFrozen
		}
		if er = deletionWorkSafe(tx, e); er != nil {
			return er
		}
		if er = protectDeletion(tx, e); er != nil {
			return er
		}
		if er = supersedeDeletionJobs(tx, e.ID); er != nil {
			return er
		}
		if er = resolveEntryConflicts(tx, e.ID); er != nil {
			return er
		}
		if r.Action == model.ResolveAcceptDelete {
			e.Generation++
			e.Status = model.StatusSynced
			// Preserve the old binding in recovery without suppressing a
			// future real restoration of this resource in the cloud.
			e.Token = ""
			return tombstone(tx, e)
		}
		if content == "" {
			content = e.Local
			if content == "" {
				content = c.Local
			}
		}
		if content == "" {
			return model.ErrUnsupportedContent
		}
		if er = s.checkBlob(content); er != nil {
			return er
		}
		if e.Kind == model.KindDocx {
			size, er := s.blobSize(content)
			if er != nil {
				return er
			}
			if size > LargeSnapshotBytes {
				return model.ErrUnsupportedContent
			}
		}
		if r.Content != nil && model.IsMarkdownFile(e) && !validPlainResolutionText(r.Content) {
			return model.ErrUnsupportedContent
		}
		e.Token = ""
		e.LocalCheckpoint = ""
		e.RemoteCheckpoint = ""
		e.Desired = ""
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		e, j, er = s.stageBlob(tx, e.ID, &r.ExpectedGeneration, content)
		if er != nil {
			return er
		}
		record, er := jobTx(tx, j.ID)
		if er != nil {
			return er
		}
		record.HumanResolution = true
		return saveJob(tx, record)
	})
	return
}

// AcknowledgeMissingDelete accepts absence only before any external delete was
// dispatched. A dispatched timeout remains unknown even when a listing is empty.
func (s *Store) AcknowledgeMissingDelete(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.Op.Kind != "delete" || r.Op.State != "finished" {
			return ErrInvalidState
		}
		if r.Dispatched {
			return model.ErrUnknownResult
		}
		if r.CloudStep != "" || len(r.CompletedSteps) != 0 {
			return ErrInvalidState
		}
		if !r.ActionPending {
			return nil
		}
		r.ActionPending = false
		return put(tx, "namespace", id, r)
	})
}

// RefreshReappearedConflict replaces an absence marker only after the caller has
// freshly observed the same cloud identity and saved its immutable snapshot.
// Draft updates are intentionally outside the CAS: the transaction retains the
// latest draft while rejecting changes to the captured conflict/local inputs.
func (s *Store) RefreshReappearedConflict(ctx context.Context, expected model.Conflict, remote model.Snapshot) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if err := get(tx, "conflicts", string(expected.ID), &c); err != nil {
			return err
		}
		if c.Resolved {
			return ErrInvalidState
		}
		if c.EntryID != expected.EntryID {
			return model.ErrIdentityChanged
		}
		e, err := entryTx(tx, c.EntryID)
		if err != nil {
			return err
		}
		if e.Generation != expected.Generation || c.Generation != expected.Generation {
			return model.ErrGenerationChanged
		}
		if c.Reason != model.ConflictRemoteDeleted || expected.Reason != model.ConflictRemoteDeleted || c.RemoteRevision != expected.RemoteRevision || c.RemoteRevision != model.DeletedRevision(e.Token) || c.Local != expected.Local || c.Base != expected.Base || c.Remote != expected.Remote || e.Local != expected.Local {
			return model.ErrVersionChanged
		}
		var saved snapshotRecord
		if err = get(tx, "snapshots", string(remote.ID), &saved); err != nil {
			return err
		}
		sn := saved.Snapshot
		if sn.Token != remote.Token || sn.Revision != remote.Revision || sn.Markdown != remote.Markdown || sn.FileData != remote.FileData || sn.XML != remote.XML || sn.BlockMap != remote.BlockMap || sn.Size != remote.Size {
			return model.ErrVersionChanged
		}
		if e.Token == "" || e.Token != sn.Token {
			return model.ErrIdentityChanged
		}
		supported := (e.Kind == model.KindFile && sn.FileData != "") || (e.Kind == model.KindDocx && sn.FileData == "" && sn.Markdown != "")
		if !supported {
			return model.ErrUnsupportedContent
		}
		var metadata Metadata
		if err = get(tx, "metadata", string(e.ID), &metadata); err != nil {
			return err
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		if err = deletionWorkSafe(tx, e); err != nil {
			return err
		}
		if err = s.checkBlob(sn.ContentBlob()); err != nil {
			return err
		}
		c.Reason = "remote_reappeared"
		c.Remote = sn.ContentBlob()
		c.RemoteRevision = sn.ConflictVersion()
		return put(tx, "conflicts", string(c.ID), c)
	})
}
