package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"reflect"
	"strconv"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

var ErrDestinationChanged = errors.New("history_destination_changed")

type HistoryRestoreBundle struct {
	Snapshot *model.Snapshot
	Blobs    map[model.BlobID]io.Reader
}
type restoreReceipt struct {
	Request model.HistoryRestoreRequest `json:"request"`
	Result  model.HistoryRestoreResult  `json:"result"`
}

func (s *Store) HistoryRestoreReceipt(ctx context.Context, r model.HistoryRestoreRequest) (result model.HistoryRestoreResult, found bool, err error) {
	if r.ProfileID != s.scope.ProfileID || r.MountID != s.scope.MountID {
		return result, false, model.ErrIdentityChanged
	}
	err = s.view(ctx, func(tx *bolt.Tx) error {
		var receipt restoreReceipt
		if e := get(tx, "history_restore", r.RequestID, &receipt); errors.Is(e, ErrNotFound) {
			return nil
		} else if e != nil {
			return e
		}
		if !reflect.DeepEqual(receipt.Request, r) {
			return ErrInvalidState
		}
		found = true
		result = receipt.Result
		return nil
	})
	return
}
func (s *Store) RestoreHistory(ctx context.Context, r model.HistoryRestoreRequest, body io.Reader, bundle *HistoryRestoreBundle) (out model.HistoryRestoreResult, err error) {
	if !canonicalUUID(r.RequestID) || r.ProfileID != s.scope.ProfileID || r.MountID != s.scope.MountID || !canonicalUUID(string(r.EntryID)) || !canonicalUUID(string(r.ParentID)) || r.VersionID == "" || validRemote(r.ExpectedPath, false) != nil {
		return out, model.ErrIdentityChanged
	}
	if saved, found, e := s.HistoryRestoreReceipt(ctx, r); e != nil || found {
		return saved, e
	}
	b, err := s.PutBlob(ctx, body)
	if err != nil {
		return out, err
	}
	var localSnapshot *model.Snapshot
	if bundle != nil && bundle.Snapshot != nil {
		for hash, reader := range bundle.Blobs {
			got, e := s.PutBlob(ctx, reader)
			if e != nil {
				return out, e
			}
			if got != hash {
				return out, ErrIntegrity
			}
		}
		prepared, e := s.prepareSnapshot(*bundle.Snapshot)
		if e != nil {
			return out, e
		}
		localSnapshot = &prepared
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var receipt restoreReceipt
		if e := get(tx, "history_restore", r.RequestID, &receipt); e == nil {
			if !reflect.DeepEqual(receipt.Request, r) {
				return ErrInvalidState
			}
			out = receipt.Result
			return nil
		} else if !errors.Is(e, ErrNotFound) {
			return e
		}
		e, er := entryTx(tx, r.EntryID)
		if er != nil {
			return er
		}
		if e.Generation != r.ExpectedGeneration {
			return model.ErrGenerationChanged
		}
		if e.Remote != r.ExpectedPath || e.ProfileID != r.ProfileID || e.MountID != r.MountID || e.Kind == model.KindFolder {
			return model.ErrIdentityChanged
		}
		var m Metadata
		if er = get(tx, "metadata", string(e.ID), &m); er != nil {
			return er
		}
		if m.Tombstone != r.ExpectedDeleted || m.SafeSaveParked || m.SafeSaveOriginal != "" || m.SafeSavePending != "" || m.VirtualType != "" {
			return ErrInvalidState
		}
		occupied, er := byPath(tx, e.Remote)
		if er == nil && occupied.ID != e.ID {
			return ErrDestinationChanged
		}
		if er != nil && !errors.Is(er, ErrNotFound) {
			return er
		}
		parentPath := path.Dir(e.Remote)
		if parentPath == "." {
			parentPath = ""
		}
		parent, er := byPath(tx, parentPath)
		if er != nil {
			return ErrDestinationChanged
		}
		if parent.ID != r.ParentID || parent.Generation != r.ParentGeneration || parent.Kind != model.KindFolder {
			return ErrDestinationChanged
		}
		if e.FrozenBy != "" || namespaceReserved(tx, e.Remote) || namespaceReserved(tx, parentPath) {
			return ErrFrozen
		}
		if er = each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if c.EntryID == e.ID && !c.Resolved {
				return ErrBusy
			}
			return nil
		}); er != nil {
			return er
		}
		if er = each[namespaceRecord](tx, "namespace", func(n namespaceRecord) error {
			if namespaceTouches(n, e) && (n.ActionPending || n.Dispatched || n.Op.State != "finished") {
				return ErrBusy
			}
			return nil
		}); er != nil {
			return er
		}
		if e.Kind == model.KindDocx {
			n, er := s.blobSize(b)
			if er != nil {
				return er
			}
			if n > LargeSnapshotBytes {
				return model.ErrUnsupportedContent
			}
		}
		if m.Tombstone {
			if hasPending(tx, e.ID) {
				return ErrBusy
			}
			e.Token = ""
			e.LocalCheckpoint = ""
			e.RemoteCheckpoint = ""
			m.Tombstone = false
			if er = tx.Bucket([]byte("paths")).Put(pathKey(e.Remote), []byte(e.ID)); er != nil {
				return er
			}
		}
		if localSnapshot != nil {
			if er = s.saveSnapshot(tx, *localSnapshot); er != nil {
				return er
			}
			m.HistorySnapshot = localSnapshot.ID
			m.HistoryMediaBody = b
		}
		if er = put(tx, "metadata", string(e.ID), m); er != nil {
			return er
		}
		// Persist any cleared cloud identity before staging; history remains in this transaction.
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		previous := tx.Bucket([]byte("history_events")).Sequence()
		updated, job, er := s.stageBlob(tx, e.ID, &r.ExpectedGeneration, b)
		if er != nil {
			return er
		}
		var event model.HistoryEvent
		if er = get(tx, "history_last", string(e.ID), &event); er != nil {
			return er
		}
		if event.Sequence <= previous {
			event.Sequence, er = tx.Bucket([]byte("history_events")).NextSequence()
			if er != nil {
				return er
			}
			event.ID = event.Source + ":" + strconv.FormatUint(event.Sequence, 10)
		}
		event.At = time.Now().UTC()
		event.Kind = "restored"
		event.RestoredFrom = r.VersionID
		event.Entry = updated
		event.Bodies["local"] = b
		raw, er := json.Marshal(event)
		if er != nil {
			return er
		}
		if er = tx.Bucket([]byte("history_events")).Put(historyKey(event.Sequence), raw); er != nil {
			return er
		}
		if er = put(tx, "history_last", string(e.ID), event); er != nil {
			return er
		}
		out = model.HistoryRestoreResult{Entry: updated, Job: job, LocalSaved: true}
		return put(tx, "history_restore", r.RequestID, restoreReceipt{r, out})
	})
	return
}
