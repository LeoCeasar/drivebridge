package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

const managedRoutesBucket = "managed_checkpoints"
const managedReceiptsBucket = "managed_checkpoint_receipts"

type managedCheckpointReceipt struct {
	Blob  model.BlobID `json:"blob"`
	Entry model.Entry  `json:"entry"`
}

func managedReceiptTx(tx *bolt.Tx, key string, seq uint64) (r managedCheckpointReceipt, err error) {
	b := tx.Bucket([]byte(managedReceiptsBucket)).Bucket([]byte(key))
	if b == nil {
		return r, ErrNotFound
	}
	raw := b.Get(historyKey(seq))
	if raw == nil {
		return r, ErrNotFound
	}
	err = json.Unmarshal(raw, &r)
	return
}
func saveManagedReceiptTx(tx *bolt.Tx, key string, seq uint64, blob model.BlobID, e model.Entry) error {
	b, err := tx.Bucket([]byte(managedReceiptsBucket)).CreateBucketIfNotExists([]byte(key))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(managedCheckpointReceipt{Blob: blob, Entry: e})
	if err != nil {
		return err
	}
	return b.Put(historyKey(seq), raw)
}

type managedCheckpointRoute struct {
	Entry        model.Entry             `json:"entry"`
	ArchiveEntry *model.Entry            `json:"archive_entry,omitempty"`
	HistoryOnly  bool                    `json:"history_only,omitempty"`
	LocalOnly    *bool                   `json:"local_only,omitempty"`
	Order        uint64                  `json:"order,omitempty"`
	Retired      bool                    `json:"retired,omitempty"`
	LastSequence uint64                  `json:"last_sequence,omitempty"`
	LastBlob     model.BlobID            `json:"last_blob,omitempty"`
	LastEntry    model.Entry             `json:"last_entry"`
	Pending      model.ManagedCheckpoint `json:"pending"`
}

func initManagedCheckpointsTx(tx *bolt.Tx) error {
	if _, err := tx.CreateBucketIfNotExists([]byte(managedReceiptsBucket)); err != nil {
		return err
	}
	if _, err := tx.CreateBucketIfNotExists([]byte(managedRoutesBucket)); err != nil {
		return err
	}
	if err := initManagedPendingIndexTx(tx); err != nil {
		return err
	}
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		if err := raiseReaderFloorTx(tx, 4); err != nil {
			return err
		}
	}
	for key, id := range refs {
		if _, err := managedRouteTx(tx, key); err == nil {
			continue
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		e, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		order, err := tx.Bucket([]byte(managedRoutesBucket)).NextSequence()
		if err != nil {
			return err
		}
		if err := saveManagedRouteTx(tx, key, managedCheckpointRoute{Entry: e, Order: order}); err != nil {
			return err
		}
	}
	return nil
}

func managedRouteTx(tx *bolt.Tx, key string) (r managedCheckpointRoute, err error) {
	err = get(tx, managedRoutesBucket, key, &r)
	return
}
func saveManagedRouteTx(tx *bolt.Tx, key string, r managedCheckpointRoute) error {
	previous, err := managedRouteTx(tx, key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err = updateManagedPendingIndexTx(tx, key, previous, r); err != nil {
		return err
	}
	return put(tx, managedRoutesBucket, key, r)
}

func retainManagedRouteTx(tx *bolt.Tx, key string, e model.Entry) error {
	if r, err := managedRouteTx(tx, key); err == nil {
		if r.Entry.ID != e.ID {
			return model.ErrIdentityChanged
		}
		r.Retired = false
		return saveManagedRouteTx(tx, key, r)
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	order, err := tx.Bucket([]byte(managedRoutesBucket)).NextSequence()
	if err != nil {
		return err
	}
	return saveManagedRouteTx(tx, key, managedCheckpointRoute{Entry: e, Order: order})
}

func fenceOlderManagedRoutesTx(tx *bolt.Tx, key string, current managedCheckpointRoute) error {
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return err
	}
	for previous, id := range refs {
		if previous == key || id != current.Entry.ID {
			continue
		}
		route, err := managedRouteTx(tx, previous)
		if err != nil {
			return err
		}
		if route.Order < current.Order {
			route.HistoryOnly = true
			if err := saveManagedRouteTx(tx, previous, route); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) ManagedCheckpointEntry(ctx context.Context, key string) (out model.Entry, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		route, er := managedRouteTx(tx, key)
		if er != nil {
			return er
		}
		out, er = entryTx(tx, route.Entry.ID)
		return er
	})
	return
}

func retireManagedRouteTx(tx *bolt.Tx, key string) error {
	r, err := managedRouteTx(tx, key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	r.Retired = true
	return saveManagedRouteTx(tx, key, r)
}

// BindManagedNamespace binds already durable save receipts, never body bytes.
func (s *Store) BindManagedNamespace(ctx context.Context, id string, inputs []model.ManagedCheckpoint) error {
	return s.update(ctx, func(tx *bolt.Tx) error { return bindManagedNamespaceTx(tx, id, inputs) })
}
func bindManagedNamespaceTx(tx *bolt.Tx, id string, inputs []model.ManagedCheckpoint) error {
	var r namespaceRecord
	if err := get(tx, "namespace", id, &r); err != nil {
		return err
	}
	if r.Op.State != "prepared" {
		return ErrInvalidState
	}
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return err
	}
	if len(inputs) > 0 {
		if err := raiseReaderFloorTx(tx, 4); err != nil {
			return err
		}
	}
	allowed := map[model.EntryID]bool{}
	for _, e := range r.Before {
		allowed[e.ID] = true
	}
	seen := map[string]bool{}
	for _, cp := range inputs {
		if !canonicalUUID(cp.Journal) || cp.Sequence == 0 || cp.Size < 0 || seen[cp.Journal] || !allowed[refs[cp.Journal]] {
			return ErrIntegrity
		}
		seen[cp.Journal] = true
		route, err := managedRouteTx(tx, cp.Journal)
		if err != nil {
			return err
		}
		if cp.Sequence >= route.Pending.Sequence {
			route.Pending = cp
			if err := saveManagedRouteTx(tx, cp.Journal, route); err != nil {
				return err
			}
		}
	}
	r.ManagedCheckpoints = inputs
	return put(tx, "namespace", id, r)
}

// CommitManagedNamespace binds durable WAL inputs and accepts namespace intent
// under one database durability barrier. Body hashing remains outside the Store.
func (s *Store) CommitManagedNamespace(ctx context.Context, id string, inputs []model.ManagedCheckpoint) (out model.Entry, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.State != "prepared" {
			if r.Op.State != "sealed" && r.Op.State != "committed" && r.Op.State != "finished" {
				return ErrInvalidState
			}
			saved, er := json.Marshal(r.ManagedCheckpoints)
			if er != nil {
				return er
			}
			if len(inputs) == 0 {
				inputs = nil
			}
			if len(r.ManagedCheckpoints) == 0 {
				saved = []byte("null")
			}
			incoming, er := json.Marshal(inputs)
			if er != nil {
				return er
			}
			if string(saved) != string(incoming) {
				return model.ErrIdentityChanged
			}
		} else if er := bindManagedNamespaceTx(tx, id, inputs); er != nil {
			return er
		}
		if _, er := sealNamespaceTx(tx, id); er != nil {
			return er
		}
		var er error
		out, er = s.commitNamespaceTx(tx, id)
		return er
	})
	return
}

func (s *Store) ManagedNamespaceCheckpoints(ctx context.Context, id string) (out []model.ManagedCheckpoint, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		out = r.ManagedCheckpoints
		return nil
	})
	return
}

func (s *Store) PendingManagedSize(ctx context.Context, id model.EntryID) (size int64, modTime time.Time, pending bool, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(managedPendingBucket)).Bucket([]byte(id))
		if b == nil {
			return nil
		}
		_, raw := b.Cursor().Last()
		if raw == nil {
			return nil
		}
		var info managedPendingMetadata
		if er := json.Unmarshal(raw, &info); er != nil {
			return er
		}
		size, modTime, pending = info.Size, info.ModTime, true
		return nil
	})
	return
}

func pendingManagedInputTx(tx *bolt.Tx, r namespaceRecord, id model.EntryID) (bool, error) {
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return false, err
	}
	for _, cp := range r.ManagedCheckpoints {
		if refs[cp.Journal] != id {
			continue
		}
		route, err := managedRouteTx(tx, cp.Journal)
		if err != nil {
			return false, err
		}
		if !route.HistoryOnly && cp.Sequence > route.LastSequence {
			return true, nil
		}
	}
	return false, nil
}

// Route changes and local namespace commit share one transaction. A producer
// that started hashing before the move resolves this routing only at acceptance.
func routeManagedNamespaceTx(tx *bolt.Tx, r namespaceRecord, result model.Entry) error {
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return err
	}
	before := map[model.EntryID]model.Entry{}
	for _, e := range r.Before {
		before[e.ID] = e
	}
	for key, id := range refs {
		old, affected := before[id]
		if !affected {
			continue
		}
		route, err := managedRouteTx(tx, key)
		if errors.Is(err, ErrNotFound) {
			route.Entry = old
		} else if err != nil {
			return err
		}
		switch {
		case r.SafeSaveBackup && id == r.Op.SrcID:
			archived := old
			route.ArchiveEntry = &archived
			route.Entry = result
			local := true
			route.LocalOnly = &local
			refs[key] = result.ID
		case r.Op.Kind == "replace" && id == r.Op.SrcID:
			route.Entry = result
			refs[key] = result.ID
			if local, configured := r.ResultLocalOnly[result.ID]; configured {
				route.LocalOnly = &local
			}
		case r.Op.Kind == "replace" && id == r.Op.DstID:
			route.Entry = old
			route.HistoryOnly = true
		case r.Op.Kind == "delete":
			route.Entry = old
			route.HistoryOnly = true
		case r.Op.Kind == "rename":
			if r.FuseSoftRemove {
				archive := old
				route.ArchiveEntry = &archive
			}
			current, err := entryTx(tx, id)
			if err != nil {
				return err
			}
			route.Entry.Remote, route.Entry.CloudName, route.Entry.Kind = current.Remote, current.CloudName, current.Kind
			if local, configured := r.ResultLocalOnly[id]; configured {
				route.LocalOnly = &local
			}
		}
		if err := saveManagedRouteTx(tx, key, route); err != nil {
			return err
		}
	}
	return put(tx, "meta", "managed_entries", refs)
}

// A historical checkpoint has its own event; it never advances history_last or
// writes the current entry. Late deleted/displaced saves cannot resurrect it.
func appendManagedHistoryTx(tx *bolt.Tx, e model.Entry, b model.BlobID, deleted bool) error {
	var sc Scope
	if err := get(tx, "meta", "scope", &sc); err != nil {
		return err
	}
	e.Local, e.Visible = b, b
	source := string(tx.Bucket([]byte("meta")).Get([]byte("history_source")))
	ev := model.HistoryEvent{Source: source, ProfileID: sc.ProfileID, MountID: sc.MountID, IdentityHash: sc.IdentityHash, Entry: e, Kind: "local_saved", At: time.Now().UTC(), Deleted: deleted, Bodies: map[string]model.BlobID{"local": b}}
	if e.LocalCheckpoint != "" {
		ev.Bodies["base_local"] = e.LocalCheckpoint
	}
	if e.RemoteCheckpoint != "" {
		var remote snapshotRecord
		if err := get(tx, "snapshots", string(e.RemoteCheckpoint), &remote); err != nil {
			return err
		}
		sn := remote.Snapshot
		ev.RemoteSnapshot = &sn
		for role, blob := range map[string]model.BlobID{"remote": sn.ContentBlob(), "remote_xml": sn.XML, "remote_block_map": sn.BlockMap} {
			if blob != "" {
				ev.Bodies[role] = blob
			}
		}
		if len(sn.DocMedia) > 0 {
			ev.Media = map[string]model.BlobID{}
			for name, blob := range sn.DocMedia {
				ev.Media["remote:"+name] = blob
				ev.Media["base_local:"+name] = blob
				ev.Media["local:"+name] = blob
			}
		}
	}
	bucket := tx.Bucket([]byte("history_events"))
	seq, err := bucket.NextSequence()
	if err != nil {
		return err
	}
	ev.Sequence, ev.ID = seq, fmt.Sprintf("%s:%d", source, seq)
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return bucket.Put(historyKey(seq), raw)
}

// AcceptManagedCheckpoint accepts a prehashed WAL checkpoint using its durable
// identity route. Per-journal receipts make retry and out-of-order completion safe.
func (s *Store) AcceptManagedCheckpoint(ctx context.Context, key string, seq uint64, b model.BlobID, localOnly bool) (out model.Entry, err error) {
	if !canonicalUUID(key) || seq == 0 || !blobName.MatchString(string(b)) {
		return out, ErrIntegrity
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		route, er := managedRouteTx(tx, key)
		if er != nil {
			return er
		}
		if receipt, er := managedReceiptTx(tx, key, seq); er == nil {
			if receipt.Blob != b {
				return model.ErrIdentityChanged
			}
			out = receipt.Entry
			return nil
		} else if !errors.Is(er, ErrNotFound) {
			return er
		}
		// Upgrade receipts written by the original journal-aware reader.
		if seq == route.LastSequence && route.LastSequence != 0 {
			if b != route.LastBlob {
				return model.ErrIdentityChanged
			}
			out = route.LastEntry
			return saveManagedReceiptTx(tx, key, seq, b, out)
		}
		if route.Retired {
			return ErrNotFound
		}
		if er = s.checkBlob(b); er != nil {
			return er
		}
		current, er := entryTx(tx, route.Entry.ID)
		if er != nil {
			return er
		}
		var metadata Metadata
		if er = get(tx, "metadata", string(current.ID), &metadata); er != nil {
			return er
		}
		stale := seq < route.LastSequence
		if route.HistoryOnly || metadata.Tombstone || stale {
			out = route.Entry
			out.Local, out.Visible = b, b
			archive := out
			deleted := metadata.Tombstone
			if route.ArchiveEntry != nil {
				archive = *route.ArchiveEntry
				// Closing a hidden/backup shadow is not deletion of its live logical file.
				var logical Metadata
				if er = get(tx, "metadata", string(archive.ID), &logical); er != nil {
					return er
				}
				deleted = logical.Tombstone && !logical.SafeSaveParked
			}
			if er = appendManagedHistoryTx(tx, archive, b, deleted); er != nil {
				return er
			}
			if stale && !route.HistoryOnly && !metadata.Tombstone {
				out = current
			}
		} else {
			receipt := false
			if route.LocalOnly != nil {
				localOnly = *route.LocalOnly
			}
			if localOnly {
				receipt, er = eligibleLocalReceipt(tx, current, "")
				if er != nil {
					return er
				}
			}
			beforeHistory := tx.Bucket([]byte("history_events")).Sequence()
			var job model.Job
			out, job, er = s.stageBlob(tx, current.ID, nil, b)
			if er != nil {
				return er
			}
			if receipt {
				if er = recordLocalReceipt(tx, job.ID); er != nil {
					return er
				}
			}
			size, er := s.blobSize(b)
			if er != nil {
				return er
			}
			// Large document text is still a durable local save. The existing upload
			// constraint blocks cloud admission instead of rejecting WAL acceptance.
			if out.Kind == model.KindDocx && size > LargeSnapshotBytes {
				rec, er := jobTx(tx, job.ID)
				if er != nil {
					return er
				}
				rec.Job.State, rec.ErrorCode, rec.ResumeState = model.StatusBlocked, "unsupported_content", model.StatusMerging
				if er = saveJob(tx, rec); er != nil {
					return er
				}
				out.Status = model.StatusBlocked
				if er = saveEntry(tx, out); er != nil {
					return er
				}
			}
			if route.ArchiveEntry != nil || tx.Bucket([]byte("history_events")).Sequence() == beforeHistory {
				archive := out
				if route.ArchiveEntry != nil {
					archive = *route.ArchiveEntry
				}
				if er = appendManagedHistoryTx(tx, archive, b, false); er != nil {
					return er
				}
			}
			if er = fenceOlderManagedRoutesTx(tx, key, route); er != nil {
				return er
			}
			if !route.Pending.ModTime.IsZero() && seq >= route.Pending.Sequence {
				if er = get(tx, "metadata", string(out.ID), &metadata); er != nil {
					return er
				}
				metadata.ModTime = route.Pending.ModTime
				if er = put(tx, "metadata", string(out.ID), metadata); er != nil {
					return er
				}
			}
		}
		if er = saveManagedReceiptTx(tx, key, seq, b, out); er != nil {
			return er
		}
		if seq > route.LastSequence {
			route.LastSequence, route.LastBlob, route.LastEntry = seq, b, out
		}
		return saveManagedRouteTx(tx, key, route)
	})
	return
}

func managedNamespaceBlockedTx(tx *bolt.Tx, r namespaceRecord) (bool, error) {
	if r.Dispatched {
		return false, nil
	}
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return false, err
	}
	affected := map[model.EntryID]bool{r.Op.SrcID: true, r.Op.DstID: true, r.Op.ResultID: true}
	for _, e := range r.Before {
		affected[e.ID] = true
	}
	delete(affected, "")
	for id := range affected {
		if inflight(tx, id, "") {
			return true, nil
		}
	}
	for _, id := range refs {
		if affected[id] {
			return true, nil
		}
	}
	creating := false
	if err := each[model.CreateOp](tx, "createops", func(c model.CreateOp) error {
		if affected[c.EntryID] && c.State != "finished" && c.State != "not_dispatched" {
			creating = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	return creating, nil
}
