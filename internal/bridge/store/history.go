package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

var historyBuckets = []string{"history_events", "history_last", "history_receipts", "history_restore", "history_media_cache"}

func historyKey(seq uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], seq); return b[:] }

func initHistoryTx(tx *bolt.Tx) error {
	for _, name := range historyBuckets {
		if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
			return err
		}
	}
	meta := tx.Bucket([]byte("meta"))
	if meta.Get([]byte("history_source")) == nil {
		if err := meta.Put([]byte("history_source"), []byte(uuid.NewString())); err != nil {
			return err
		}
	}
	return nil
}

// captureHistoryTx runs inside the transaction that accepted the entry change.
// It only reads metadata and references; it never opens files, Git or a network.
func captureHistoryTx(tx *bolt.Tx, id model.EntryID, conflict *model.Conflict) error {
	if tx.Bucket([]byte("history_events")) == nil {
		return ErrIntegrity
	}
	e, err := entryTx(tx, id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var m Metadata
	if err = get(tx, "metadata", string(id), &m); errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.SafeSaveParked || m.SafeSaveOriginal != "" {
		return nil
	}
	var previous model.HistoryEvent
	oldErr := get(tx, "history_last", string(id), &previous)
	if oldErr != nil && !errors.Is(oldErr, ErrNotFound) {
		return oldErr
	}
	// Entry/metadata writes and conflict writes are separate capture hooks in
	// the same transaction. Retain the active conflict on both paths, otherwise
	// an unchanged refresh alternates between "no conflict" and "draft saved".
	if conflict == nil {
		retain := func(c model.Conflict) error {
			if c.EntryID == e.ID && c.Generation == e.Generation && !c.Resolved {
				copy := c
				conflict = &copy
			}
			return nil
		}
		if oldErr == nil && previous.Conflict != nil {
			var current model.Conflict
			if err := get(tx, "conflicts", string(previous.Conflict.ID), &current); err == nil {
				_ = retain(current)
			} else if !errors.Is(err, ErrNotFound) {
				return err
			}
		} else if oldErr != nil {
			// Only first capture/legacy migration needs the conflict inventory.
			if err := each[model.Conflict](tx, "conflicts", retain); err != nil {
				return err
			}
		}
	}
	if e.Token == "" {
		for _, p := range []string{"*~", "*~.*", ".~*", ".DS_Store"} {
			if match, _ := path.Match(p, path.Base(e.Remote)); match {
				return nil
			}
		}
	}
	var sc Scope
	if err = get(tx, "meta", "scope", &sc); err != nil {
		return err
	}
	ev := model.HistoryEvent{Source: string(tx.Bucket([]byte("meta")).Get([]byte("history_source"))), ProfileID: sc.ProfileID, MountID: sc.MountID, IdentityHash: sc.IdentityHash, Entry: e, At: time.Now().UTC(), Deleted: m.Tombstone, Bodies: map[string]model.BlobID{}, Media: map[string]model.BlobID{}, Conflict: conflict}
	for role, b := range map[string]model.BlobID{"local": e.Local, "base_local": e.LocalCheckpoint} {
		if b != "" {
			ev.Bodies[role] = b
		}
	}
	if e.RemoteCheckpoint != "" {
		var sn snapshotRecord
		if err = get(tx, "snapshots", string(e.RemoteCheckpoint), &sn); err != nil {
			return err
		}
		v := sn.Snapshot
		if v.XML != "" && len(v.DocMedia) == 0 {
			var siblingID string
			if get(tx, "history_media_cache", historyMediaKey(v), &siblingID) == nil {
				var sibling snapshotRecord
				if get(tx, "snapshots", siblingID, &sibling) == nil {
					v = sibling.Snapshot
				}
			}
		}
		ev.RemoteSnapshot = &v
		for role, b := range map[string]model.BlobID{"remote": v.ContentBlob(), "remote_xml": v.XML, "remote_block_map": v.BlockMap} {
			if b != "" {
				ev.Bodies[role] = b
			}
		}
		for name, b := range v.DocMedia {
			ev.Media["remote:"+name] = b
			ev.Media["base_local:"+name] = b
		}
	}
	if m.HistorySnapshot != "" && m.HistoryMediaBody == e.Local {
		var local snapshotRecord
		if err := get(tx, "snapshots", string(m.HistorySnapshot), &local); err != nil {
			return err
		}
		v := local.Snapshot
		ev.LocalSnapshot = &v
		for name, b := range v.DocMedia {
			ev.Media["local:"+name] = b
		}
		for role, b := range map[string]model.BlobID{"local_xml": v.XML, "local_block_map": v.BlockMap, "local_source": v.ContentBlob()} {
			if b != "" {
				ev.Bodies[role] = b
			}
		}
	} else if ev.RemoteSnapshot != nil {
		for name, b := range ev.RemoteSnapshot.DocMedia {
			ev.Media["local:"+name] = b
		}
	}
	if conflict != nil {
		var selected snapshotRecord
		if err := each[snapshotRecord](tx, "snapshots", func(r snapshotRecord) error {
			if r.Snapshot.ContentBlob() == conflict.Remote && r.Snapshot.ConflictVersion() == conflict.RemoteRevision && r.SavedAt.After(selected.SavedAt) {
				selected = r
			}
			return nil
		}); err != nil {
			return err
		}
		if selected.Snapshot.ID != "" {
			sn := selected.Snapshot
			ev.ConflictRemoteSnapshot = &sn
			for role, b := range map[string]model.BlobID{"conflict_remote_xml": sn.XML, "conflict_remote_block_map": sn.BlockMap} {
				if b != "" {
					ev.Bodies[role] = b
				}
			}
			for name, b := range sn.DocMedia {
				ev.Media["conflict_remote:"+name] = b
			}
		}
		for key, b := range ev.Media {
			if strings.HasPrefix(key, "local:") {
				ev.Media["draft:"+strings.TrimPrefix(key, "local:")] = b
			}
		}
	}
	if conflict != nil {
		for role, b := range map[string]model.BlobID{"conflict_base": conflict.Base, "conflict_local": conflict.Local, "conflict_remote": conflict.Remote, "draft": conflict.Draft} {
			if b != "" {
				ev.Bodies[role] = b
			}
		}
	}
	// Cache eviction changes availability, never the historical content/version.
	if oldErr == nil && e.Local == "" && previous.Entry.Local != "" && e.Generation == previous.Entry.Generation && !m.Tombstone && e.Remote == previous.Entry.Remote {
		return nil
	}
	fingerprint := func(v model.HistoryEvent) string {
		// Omitted empty resource maps decode as nil. Normalize both sides so
		// an ordinary metadata refresh does not appear to change resources.
		if len(v.Media) == 0 {
			v.Media = nil
		}
		if len(v.Bodies) == 0 {
			v.Bodies = nil
		}
		revision := ""
		if v.RemoteSnapshot != nil {
			revision = v.RemoteSnapshot.Revision
			if v.RemoteSnapshot.FileIdentity != nil {
				revision = v.RemoteSnapshot.FileIdentity.Version
			}
		}
		raw, _ := json.Marshal(struct {
			Path, Token, Revision string
			Kind                  model.Kind
			Deleted               bool
			Bodies, Media         map[string]model.BlobID
			Conflict              *model.Conflict
		}{v.Entry.Remote, v.Entry.Token, revision, v.Entry.Kind, v.Deleted, v.Bodies, v.Media, v.Conflict})
		h := sha256.Sum256(raw)
		return hex.EncodeToString(h[:])
	}
	if oldErr == nil && fingerprint(previous) == fingerprint(ev) {
		return nil
	}
	ev.Kind = "discovered"
	if oldErr == nil {
		ev.Kind = "state_updated"
	}
	switch {
	case conflict != nil && conflict.Resolved:
		ev.Kind = "conflict_resolved"
	case conflict != nil:
		ev.Kind = "conflict_opened"
		if conflict.Draft != "" {
			ev.Kind = "draft_saved"
		}
	case ev.Deleted:
		ev.Kind = "deleted"
	case oldErr == nil && previous.Entry.Remote != e.Remote:
		ev.Kind = "renamed"
	case e.Local != "" && (oldErr != nil || previous.Entry.Local != e.Local):
		ev.Kind = "local_saved"
		if e.Status == model.StatusSynced {
			ev.Kind = "materialized"
		}
	case e.LocalCheckpoint != "" && e.LocalCheckpoint != previous.Entry.LocalCheckpoint:
		ev.Kind = "upload_verified"
	case e.RemoteCheckpoint != "" && e.RemoteCheckpoint != previous.Entry.RemoteCheckpoint:
		ev.Kind = "remote_observed"
	}
	bucket := tx.Bucket([]byte("history_events"))
	seq, err := bucket.NextSequence()
	if err != nil {
		return err
	}
	ev.Sequence = seq
	ev.ID = fmt.Sprintf("%s:%d", ev.Source, seq)
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err = bucket.Put(historyKey(seq), raw); err != nil {
		return err
	}
	return put(tx, "history_last", string(id), ev)
}

func (s *Store) HistoryEvents(ctx context.Context, after uint64, limit int) (out []model.HistoryEvent, err error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	err = s.view(ctx, func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("history_events")).Cursor()
		for k, v := c.Seek(historyKey(after + 1)); k != nil && len(out) < limit; k, v = c.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var ev model.HistoryEvent
			if err := json.Unmarshal(v, &ev); err != nil {
				return err
			}
			ev.Archived = tx.Bucket([]byte("history_receipts")).Get(k) != nil
			out = append(out, ev)
		}
		return nil
	})
	return
}
func (s *Store) AckHistory(ctx context.Context, seq uint64, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("history_events")).Get(historyKey(seq))
		if raw == nil {
			return ErrNotFound
		}
		var e model.HistoryEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if e.ID != id {
			return model.ErrIdentityChanged
		}
		m := tx.Bucket([]byte("meta"))
		var ack uint64
		if b := m.Get([]byte("history_ack")); len(b) == 8 {
			ack = binary.BigEndian.Uint64(b)
		}
		if seq > ack+1 {
			return ErrInvalidState
		}
		if err := tx.Bucket([]byte("history_receipts")).Put(historyKey(seq), []byte(id)); err != nil {
			return err
		}
		if seq == ack+1 {
			return m.Put([]byte("history_ack"), historyKey(seq))
		}
		return nil
	})
}
func historyProtectedTx(tx *bolt.Tx, mark func(model.BlobID)) error {
	b := tx.Bucket([]byte("history_events"))
	if b == nil {
		return nil
	}
	return b.ForEach(func(k, v []byte) error {
		if tx.Bucket([]byte("history_receipts")).Get(k) != nil {
			return nil
		}
		var ev model.HistoryEvent
		if err := json.Unmarshal(v, &ev); err != nil {
			return err
		}
		for _, b := range ev.Blobs() {
			mark(b)
		}
		return nil
	})
}

// HistoryCursor returns the producer identity and contiguous archive receipts.
func (s *Store) HistoryCursor(ctx context.Context) (source string, ack uint64, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		m := tx.Bucket([]byte("meta"))
		source = string(m.Get([]byte("history_source")))
		v := m.Get([]byte("history_ack"))
		if len(v) == 8 {
			ack = binary.BigEndian.Uint64(v)
		}
		return nil
	})
	return
}

func (s *Store) HistoryStatus(ctx context.Context) (out model.HistoryStatus, err error) {
	out.State = "synced"
	err = s.view(ctx, func(tx *bolt.Tx) error {
		m := tx.Bucket([]byte("meta"))
		var ack uint64
		if b := m.Get([]byte("history_ack")); len(b) == 8 {
			ack = binary.BigEndian.Uint64(b)
		}
		out.Archived = ack
		last := tx.Bucket([]byte("history_events")).Sequence()
		if last > ack {
			out.Pending = last - ack
			out.State = "pending"
			_, v := tx.Bucket([]byte("history_events")).Cursor().Seek(historyKey(ack + 1))
			if v != nil {
				var ev model.HistoryEvent
				if err := json.Unmarshal(v, &ev); err != nil {
					return err
				}
				out.OldestAt = ev.At
			}
		}
		return nil
	})
	return
}
func (s *Store) HistoryWake() <-chan struct{} { s.mu.Lock(); defer s.mu.Unlock(); return s.changed }

func historyMediaKey(sn model.Snapshot) string {
	raw, _ := json.Marshal([]string{sn.Token, sn.Revision, string(sn.XML), string(sn.Markdown), string(sn.BlockMap)})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func indexHistoryMediaTx(tx *bolt.Tx, sn model.Snapshot) error {
	if len(sn.DocMedia) == 0 || tx.Bucket([]byte("history_media_cache")) == nil {
		return nil
	}
	return put(tx, "history_media_cache", historyMediaKey(sn), string(sn.ID))
}

// HistoryProtection captures the inputs still required by live sync/recovery.
// It does not need the bytes of acknowledged versions to remain in this Store.
func (s *Store) HistoryProtection(ctx context.Context) (out model.HistoryProtection, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		out.Source = string(meta.Get([]byte("history_source")))
		if b := meta.Get([]byte("history_ack")); len(b) == 8 {
			out.Acknowledged = binary.BigEndian.Uint64(b)
		}
		needed := map[model.EntryID]map[model.BlobID]bool{}
		mark := func(id model.EntryID, blobs ...model.BlobID) {
			if needed[id] == nil {
				needed[id] = map[model.BlobID]bool{}
			}
			for _, b := range blobs {
				if b != "" {
					needed[id][b] = true
				}
			}
		}
		entry := func(e model.Entry) { mark(e.ID, e.Local, e.Visible, e.LocalCheckpoint, e.Desired) }
		if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
			if !r.Superseded && !r.LocalReceipt && r.Job.State != model.StatusSynced {
				mark(r.Job.EntryID, r.Job.Local, r.Job.BaseLocal, r.Desired)
				if r.Job.BaseRemote != "" {
					var sn snapshotRecord
					if err := get(tx, "snapshots", string(r.Job.BaseRemote), &sn); err != nil {
						return err
					}
					mark(r.Job.EntryID, sn.Snapshot.ContentBlob())
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if !c.Resolved {
				mark(c.EntryID, c.Base, c.Local, c.Remote, c.Draft)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
			if r.ActionPending || r.Dispatched || r.Op.State != "finished" || r.SafeSaveBackup && !r.SafeSaveCompleted {
				for _, e := range r.Before {
					entry(e)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if err := each[recoveryRecord](tx, "recovery", func(r recoveryRecord) error {
			for _, e := range r.Entries {
				entry(e)
			}
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket([]byte("history_events")).ForEach(func(_, raw []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var e model.HistoryEvent
			if err := json.Unmarshal(raw, &e); err != nil {
				return err
			}
			for _, b := range e.Bodies {
				if needed[e.Entry.ID][b] {
					out.Versions = append(out.Versions, e.ID)
					break
				}
			}
			return nil
		})
	})
	return
}

func (s *Store) PendingHistoryBytes(ctx context.Context) (bytes int64, err error) {
	seen := map[model.BlobID]bool{}
	err = s.view(ctx, func(tx *bolt.Tx) error { return historyProtectedTx(tx, func(b model.BlobID) { seen[b] = true }) })
	if err != nil {
		return
	}
	for b := range seen {
		if err = ctx.Err(); err != nil {
			return
		}
		var n int64
		n, err = s.blobSize(b)
		if err != nil {
			return
		}
		bytes += n
	}
	return
}
