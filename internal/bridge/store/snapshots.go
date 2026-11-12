package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

type snapshotRecord struct {
	Snapshot model.Snapshot `json:"snapshot"`
	SavedAt  time.Time      `json:"saved_at"`
}

func snapshotContent(sn model.Snapshot) model.BlobID {
	if sn.FileData != "" {
		return sn.FileData
	}
	return sn.Markdown
}
func (s *Store) blobSize(b model.BlobID) (int64, error) {
	f, err := os.Stat(s.blobPath(b))
	if err != nil {
		return 0, err
	}
	return f.Size(), nil
}
func (s *Store) prepareSnapshot(sn model.Snapshot) (model.Snapshot, error) {
	if sn.Token == "" {
		return sn, errors.New("snapshot missing token")
	}
	if sn.ID == "" {
		sn.ID = model.SnapshotID(uuid.NewString())
	}
	if !canonicalUUID(string(sn.ID)) {
		return sn, errors.New("invalid snapshot ID")
	}
	for _, b := range sn.Blobs() {
		if err := s.checkBlob(b); err != nil {
			return sn, err
		}
	}
	for name, b := range sn.DocMedia {
		if name == "" || b == "" || sn.XML == "" {
			return sn, errors.New("invalid document media reference")
		}
	}
	if sn.FileData != "" || sn.FileIdentity != nil {
		if sn.FileData == "" || sn.FileIdentity == nil || sn.XML != "" || sn.Markdown != "" || sn.BlockMap != "" {
			return sn, errors.New("mixed or incomplete file snapshot")
		}
		if err := sn.FileIdentity.Validate(); err != nil {
			return sn, err
		}
		if sn.FileIdentity.SHA256 != string(sn.FileData) {
			return sn, errors.New("file snapshot SHA does not match bytes")
		}
		size, err := s.blobSize(sn.FileData)
		if err != nil {
			return sn, err
		}
		if sn.Size != size {
			return sn, errors.New("file snapshot size mismatch")
		}
		if sn.Revision != "" {
			return sn, errors.New("ordinary file identity does not use document revision")
		}
	}
	if snapshotContent(sn) == "" {
		return sn, errors.New("snapshot has no content")
	}
	return sn, nil
}
func (s *Store) saveSnapshot(tx *bolt.Tx, sn model.Snapshot) error {
	// The earlier preparation check runs outside the Store lock. Recheck when
	// accepting references so a concurrent clean eviction cannot leave dangling
	// snapshot edges when callers reuse older immutable blobs.
	for _, b := range sn.Blobs() {
		if err := s.checkBlob(b); err != nil {
			return err
		}
	}
	var old snapshotRecord
	err := get(tx, "snapshots", string(sn.ID), &old)
	if err == nil {
		a, _ := json.Marshal(old.Snapshot)
		b, _ := json.Marshal(sn)
		if string(a) != string(b) {
			return errors.New("immutable snapshot changed")
		}
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return put(tx, "snapshots", string(sn.ID), snapshotRecord{sn, time.Now().UTC()})
}

// SaveSnapshot durably binds already-imported document blobs; ordinary files use SaveFileSnapshot.
func (s *Store) SaveSnapshot(ctx context.Context, sn model.Snapshot) (model.Snapshot, error) {
	sn, err := s.prepareSnapshot(sn)
	if err != nil {
		return sn, err
	}
	err = s.update(ctx, func(tx *bolt.Tx) error { return s.saveSnapshot(tx, sn) })
	return sn, err
}
func (s *Store) Snapshot(ctx context.Context, id model.SnapshotID) (sn model.Snapshot, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		var r snapshotRecord
		if er := get(tx, "snapshots", string(id), &r); er != nil {
			return er
		}
		sn = r.Snapshot
		return nil
	})
	return
}
func (s *Store) SaveFileSnapshot(ctx context.Context, token string, identity model.FileIdentity, src io.Reader) (model.Snapshot, error) {
	if token == "" {
		return model.Snapshot{}, errors.New("missing file token")
	}
	if err := identity.Validate(); err != nil {
		return model.Snapshot{}, err
	}
	b, err := s.PutBlob(ctx, src)
	if err != nil {
		return model.Snapshot{}, err
	}
	if identity.SHA256 != string(b) {
		return model.Snapshot{}, errors.New("observed hash mismatch")
	}
	size, err := s.blobSize(b)
	if err != nil {
		return model.Snapshot{}, err
	}
	return s.SaveSnapshot(ctx, model.Snapshot{Token: token, FileData: b, FileIdentity: &identity, Size: size, ModTime: identity.CompletedAt})
}
func (s *Store) AdoptFileSnapshot(ctx context.Context, id model.EntryID, expected uint64, snapshotID model.SnapshotID) (e model.Entry, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		e, er = entryTx(tx, id)
		if er != nil {
			return er
		}
		if e.Generation != expected {
			return model.ErrGenerationChanged
		}
		var metadata Metadata
		if er = get(tx, "metadata", string(id), &metadata); er != nil {
			return er
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		if e.Kind != model.KindFile {
			return model.ErrUnsupportedContent
		}
		if e.FrozenBy != "" {
			return ErrFrozen
		}
		if hasPending(tx, id) || e.Local != "" || e.Visible != "" || e.RemoteCheckpoint != "" {
			return ErrBusy
		}
		var r snapshotRecord
		if er = get(tx, "snapshots", string(snapshotID), &r); er != nil {
			return er
		}
		if r.Snapshot.FileData == "" {
			return model.ErrUnsupportedContent
		}
		if e.Token == "" || e.Token != r.Snapshot.Token {
			return model.ErrIdentityChanged
		}
		e.Local = r.Snapshot.FileData
		e.Visible = e.Local
		e.LocalCheckpoint = e.Local
		e.RemoteCheckpoint = snapshotID
		e.Status = model.StatusSynced
		return saveEntry(tx, e)
	})
	return
}

type recoveryRecord struct {
	JobID       model.JobID      `json:"job_id"`
	SnapshotID  model.SnapshotID `json:"snapshot_id"`
	NamespaceID string           `json:"namespace_id,omitempty"`
	Entries     []model.Entry    `json:"entries,omitempty"`
}

func (s *Store) ProtectBackup(ctx context.Context, jobID model.JobID, snapshotID model.SnapshotID) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		j, err := jobTx(tx, jobID)
		if err != nil {
			return err
		}
		var sn snapshotRecord
		if err = get(tx, "snapshots", string(snapshotID), &sn); err != nil {
			return err
		}
		e, err := entryTx(tx, j.Job.EntryID)
		if err != nil {
			return err
		}
		if e.Token != "" && sn.Snapshot.Token != e.Token {
			return model.ErrIdentityChanged
		}
		return put(tx, "recovery", string(jobID)+":"+string(snapshotID), recoveryRecord{JobID: jobID, SnapshotID: snapshotID})
	})
}
