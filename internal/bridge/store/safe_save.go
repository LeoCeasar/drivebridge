package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// Only a fully confirmed ordinary file may enter this local backup transition.
// No pending mutation or unpublished result is cancelled or hidden.
func cleanSafeSaveSource(tx *bolt.Tx, e model.Entry, namespace string) (bool, error) {
	if e.Kind != model.KindFile || e.Token == "" || e.RemoteCheckpoint == "" || e.LocalCheckpoint == "" || e.Local != e.LocalCheckpoint || e.Visible != e.LocalCheckpoint || e.Status != model.StatusSynced || hasPending(tx, e.ID) || (e.FrozenBy != "" && e.FrozenBy != namespace) {
		return false, nil
	}
	safe := true
	if err := each[publication](tx, "publications", func(p publication) error {
		if p.Candidate.EntryID == e.ID {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
		if c.EntryID == e.ID {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	if err := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
		if r.Op.ID != namespace && (r.ActionPending || r.Dispatched) && (r.Op.SrcID == e.ID || r.Op.DstID == e.ID || r.Op.ResultID == e.ID) {
			safe = false
		}
		return nil
	}); err != nil {
		return false, err
	}
	return safe, nil
}

// Unlike a remote rename, a local editor backup may preserve an upload that
// is queued, blocked or already in flight. It neither cancels nor replays it.
func editorBackupSource(tx *bolt.Tx, e model.Entry, namespace string) (bool, error) {
	return editorBackupSourceStateTx(tx, e, namespace, false)
}

func editorBackupSourceStateTx(tx *bolt.Tx, e model.Entry, namespace string, allowPending bool) (bool, error) {
	if (e.Kind != model.KindFile && e.Kind != model.KindDocx) || e.Token == "" || e.Local == "" || e.Visible != e.Local || e.RemoteCheckpoint == "" || e.LocalCheckpoint == "" || (e.FrozenBy != "" && e.FrozenBy != namespace) {
		return false, nil
	}
	var metadata Metadata
	if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return false, err
	}
	return !metadata.Tombstone && (allowPending || metadata.SafeSavePending == "") && !metadata.SafeSaveParked, nil
}

// The next editor cycle can begin before the replacement's background archive.
// A live identity-bound WAL already protects its bytes. Close the previous
// causal backup in the same transaction as the new reservation; parked originals
// and displaced/history-only journals cannot supply that proof.
func completeManagedEditorCycleTx(tx *bolt.Tx, e model.Entry) error {
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return err
	}
	protected := false
	for key, id := range refs {
		if id != e.ID {
			continue
		}
		route, err := managedRouteTx(tx, key)
		if err != nil {
			return err
		}
		if !route.HistoryOnly && !route.Retired && route.Entry.ID == e.ID {
			protected = true
			break
		}
	}
	if !protected {
		return nil
	}
	valid, err := editorBackupSourceStateTx(tx, e, "", true)
	if err != nil {
		return err
	}
	if !valid {
		return nil
	}
	return completeSafeSave(tx, e)
}

func (s *Store) ConfigureSafeSaveBackup(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.Op.Kind != "rename" || r.Op.State != "prepared" || r.Op.DstExists {
			return ErrInvalidState
		}
		e, err := entryTx(tx, r.Op.SrcID)
		if err != nil {
			return err
		}
		safe, err := cleanSafeSaveSource(tx, e, id)
		if err != nil {
			return err
		}
		if !safe {
			return ErrBusy
		}
		r.SafeSaveBackup = true
		r.LocalReceipt = false
		return put(tx, "namespace", id, r)
	})
}
func commitSafeSaveBackup(tx *bolt.Tx, r namespaceRecord) (model.Entry, error) {
	original, err := entryTx(tx, r.Op.SrcID)
	if err != nil {
		return model.Entry{}, err
	}
	var safe bool
	if model.EditorTildeBackup(r.Op.Src, r.Op.Dst) {
		safe, err = editorBackupSource(tx, original, r.Op.ID)
	} else {
		safe, err = cleanSafeSaveSource(tx, original, r.Op.ID)
	}
	if err != nil {
		return model.Entry{}, err
	}
	if !safe {
		return model.Entry{}, ErrBusy
	}
	var metadata Metadata
	if err = get(tx, "metadata", string(original.ID), &metadata); err != nil {
		return model.Entry{}, err
	}
	// Retain the original identity/token/checkpoints, but remove its local name
	// until the editor creates or renames the replacement into that exact name.
	metadata.Tombstone = true
	metadata.SafeSaveParked = true
	metadata.SafeSavePending = r.Op.ID
	original.FrozenBy = ""
	if err = put(tx, "metadata", string(original.ID), metadata); err != nil {
		return model.Entry{}, err
	}
	if err = tx.Bucket([]byte("paths")).Delete([]byte(original.Remote)); err != nil {
		return model.Entry{}, err
	}
	if err = saveEntry(tx, original); err != nil {
		return model.Entry{}, err
	}
	if r.Op.DstExists {
		previous, err := entryTx(tx, r.Op.DstID)
		if err != nil {
			return model.Entry{}, err
		}
		var prior Metadata
		if err = get(tx, "metadata", string(previous.ID), &prior); err != nil {
			return model.Entry{}, err
		}
		if previous.Token != "" || prior.SafeSaveOriginal != original.ID {
			return model.Entry{}, ErrInvalidState
		}
		if err = tombstone(tx, previous); err != nil {
			return model.Entry{}, err
		}
	}
	now := time.Now().UTC()
	shadow := model.Entry{ID: model.EntryID(uuid.NewString()), MountID: original.MountID, ProfileID: original.ProfileID, Remote: r.Op.Dst, CloudName: r.Op.Dst, Kind: original.Kind, Generation: 1, Local: original.Local, Visible: original.Local, Status: model.StatusQueued}
	if err = put(tx, "metadata", string(shadow.ID), Metadata{FirstObserved: now, ModTime: metadata.ModTime, SafeSaveOriginal: original.ID}); err != nil {
		return model.Entry{}, err
	}
	if err = saveEntry(tx, shadow); err != nil {
		return model.Entry{}, err
	}
	if err = tx.Bucket([]byte("paths")).Put([]byte(shadow.Remote), []byte(shadow.ID)); err != nil {
		return model.Entry{}, err
	}
	job := model.Job{ID: model.JobID(uuid.NewString()), EntryID: shadow.ID, Generation: shadow.Generation, Local: shadow.Local, State: model.StatusQueued}
	if err = saveJob(tx, jobRecord{Job: job, LocalReceipt: true, Superseded: true, StagedAt: now}); err != nil {
		return model.Entry{}, err
	}
	return shadow, nil
}
func parkedSafeSave(tx *bolt.Tx, remote string) (out model.Entry, found bool, err error) {
	err = eachIndexedEntryTx(tx, entryRemotes, remote, func(e model.Entry) error {
		var metadata Metadata
		if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
			return err
		}
		if !metadata.SafeSaveParked {
			return nil
		}
		if !metadata.Tombstone || e.Token == "" || found {
			return ErrInvalidState
		}
		out = e
		found = true
		return nil
	})
	return
}
func restoreSafeSaveMapping(tx *bolt.Tx, e model.Entry) error {
	var metadata Metadata
	if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return err
	}
	if !metadata.SafeSaveParked || !metadata.Tombstone {
		return ErrInvalidState
	}
	metadata.SafeSaveParked = false
	metadata.Tombstone = false
	if err := put(tx, "metadata", string(e.ID), metadata); err != nil {
		return err
	}
	return tx.Bucket([]byte("paths")).Put([]byte(e.Remote), []byte(e.ID))
}

// ResumeSafeSave is local-create admission, never cloud-listing resurrection.
// The persisted parked identity survives crashes and backup-shadow cleanup.
func (s *Store) ResumeSafeSave(ctx context.Context, remote string, kind model.Kind) (out model.Entry, resumed bool, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		out, resumed, er = parkedSafeSave(tx, remote)
		if er != nil || !resumed {
			return er
		}
		if out.Kind != kind {
			return model.ErrIdentityChanged
		}
		if out.FrozenBy != "" || namespaceReserved(tx, remote) {
			return ErrFrozen
		}
		if _, er = byPath(tx, remote); er == nil {
			return ErrBusy
		} else if er != ErrNotFound {
			return er
		}
		if er = restoreSafeSaveMapping(tx, out); er != nil {
			return er
		}
		return saveEntry(tx, out)
	})
	return
}

// ErrSafeSavePending means a causal backup rename is durable but its replacement
// has not yet been accepted. It must remain visible and must never dispatch.
var ErrSafeSavePending = errors.New("safe_save_pending: awaiting durable replacement")

func completeSafeSave(tx *bolt.Tx, e model.Entry) error {
	var metadata Metadata
	if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return err
	}
	if metadata.SafeSavePending == "" {
		return nil
	}
	if metadata.SafeSaveParked || metadata.Tombstone {
		return ErrFrozen
	}
	var r namespaceRecord
	if err := get(tx, "namespace", metadata.SafeSavePending, &r); err != nil {
		return err
	}
	if !r.SafeSaveBackup || r.Op.State != "finished" || r.Op.SrcID != e.ID || r.Op.Src != e.Remote || r.Dispatched {
		return ErrInvalidState
	}
	r.SafeSaveCompleted = true
	r.ActionPending = false
	if err := put(tx, "namespace", r.Op.ID, r); err != nil {
		return err
	}
	metadata.SafeSavePending = ""
	return put(tx, "metadata", string(e.ID), metadata)
}

// AcceptSafeSaveReplacement also handles an idempotent create that accepted the
// already-durable baseline bytes without needing a new content generation.
func (s *Store) AcceptSafeSaveReplacement(ctx context.Context, id model.EntryID, generation uint64) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		e, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		if e.Generation != generation {
			return model.ErrGenerationChanged
		}
		if e.FrozenBy != "" {
			return ErrFrozen
		}
		return completeSafeSave(tx, e)
	})
}
