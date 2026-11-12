package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func (s *Store) SaveConflict(ctx context.Context, c model.Conflict) error {
	// Resolution has its own generation-checked transition. A refresh may only
	// create an unresolved conflict or update the inputs of an existing one.
	if c.Resolved {
		return ErrInvalidState
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		if c.ID == "" {
			c.ID = model.ConflictID(uuid.NewString())
		}
		var old model.Conflict
		if er := get(tx, "conflicts", string(c.ID), &old); er == nil {
			if old.EntryID != c.EntryID {
				return model.ErrIdentityChanged
			}
			if old.Resolved {
				return ErrInvalidState
			}
			// An older refresh cannot turn a durable deletion decision back
			// into an ordinary content conflict.
			if (old.Reason == model.ConflictRemoteDeleted || old.Reason == model.ConflictLocalDeletePending) && c.Reason != old.Reason {
				return model.ErrVersionChanged
			}
			// SaveDraft owns the draft field. A remote refresh can have waited on
			// network I/O while the user saved a newer draft in this same record.
			c.Draft = old.Draft
		} else if !errors.Is(er, ErrNotFound) {
			return er
		}
		e, er := entryTx(tx, c.EntryID)
		if er != nil {
			return er
		}
		var metadata Metadata
		if er = get(tx, "metadata", string(e.ID), &metadata); er != nil {
			return er
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		if e.Generation != c.Generation {
			return model.ErrGenerationChanged
		}
		for _, b := range []model.BlobID{c.Base, c.Local, c.Remote, c.Draft} {
			if er := s.checkBlob(b); er != nil {
				return er
			}
		}
		e.Status = model.StatusConflict
		if er = saveEntry(tx, e); er != nil {
			return er
		}
		if er = each[jobRecord](tx, "jobs", func(j jobRecord) error {
			if j.Job.EntryID == c.EntryID && j.Job.Generation == c.Generation && pending(j.Job.State) {
				j.Job.State = model.StatusConflict
				return saveJob(tx, j)
			}
			return nil
		}); er != nil {
			return er
		}
		return put(tx, "conflicts", string(c.ID), c)
	})
}
func (s *Store) Conflicts(ctx context.Context) (out []model.Conflict, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if !c.Resolved {
				out = append(out, c)
			}
			return nil
		})
	})
	return
}
func (s *Store) Resolve(ctx context.Context, r model.ResolveRequest) (model.Entry, model.Job, error) {
	return s.resolve(ctx, r, nil)
}

// ResolveAgainst adopts the freshly checked remote as the explicit human merge
// baseline in the same transaction as staging the resolution generation.
func (s *Store) ResolveAgainst(ctx context.Context, r model.ResolveRequest, remote model.Snapshot) (model.Entry, model.Job, error) {
	saved, err := s.Snapshot(ctx, remote.ID)
	if err != nil {
		return model.Entry{}, model.Job{}, err
	}
	if saved.Token != remote.Token || saved.Revision != remote.Revision || saved.Markdown != remote.Markdown || saved.FileData != remote.FileData {
		return model.Entry{}, model.Job{}, model.ErrVersionChanged
	}
	if saved.FileData != "" {
		reader, err := s.ReadBlob(ctx, saved.FileData)
		if err != nil {
			return model.Entry{}, model.Job{}, err
		}
		body, readErr := io.ReadAll(io.LimitReader(reader, LargeSnapshotBytes+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			return model.Entry{}, model.Job{}, errors.Join(readErr, closeErr)
		}
		if !validPlainResolutionText(body) {
			return model.Entry{}, model.Job{}, model.ErrUnsupportedContent
		}
	}
	return s.resolve(ctx, r, &saved)
}
func (s *Store) resolve(ctx context.Context, r model.ResolveRequest, remote *model.Snapshot) (e model.Entry, j model.Job, err error) {
	b, err := s.PutBlob(ctx, bytes.NewReader(r.Content))
	if err != nil {
		return
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if er := get(tx, "conflicts", string(r.ConflictID), &c); er != nil {
			return er
		}
		if c.Resolved || c.Reason == model.ConflictRemoteDeleted || c.Reason == model.ConflictLocalDeletePending {
			return ErrInvalidState
		}
		if c.RemoteRevision != r.ExpectedRemoteRevision {
			return model.ErrVersionChanged
		}
		current, er := entryTx(tx, c.EntryID)
		if er != nil {
			return er
		}
		if current.Generation != r.ExpectedGeneration || c.Generation != current.Generation {
			return model.ErrGenerationChanged
		}
		if current.FrozenBy != "" {
			return ErrFrozen
		}
		if inflight(tx, current.ID, "") {
			return model.ErrUnknownResult
		}
		if remote != nil {
			supported := (current.Kind == model.KindDocx && remote.FileData == "") || (model.IsMarkdownFile(current) && remote.FileData != "")
			if !supported || remote.Token != current.Token || remote.ConflictVersion() != r.ExpectedRemoteRevision || remote.ContentBlob() == "" || remote.ContentBlob() != c.Remote {
				return model.ErrVersionChanged
			}
			current.LocalCheckpoint = remote.ContentBlob()
			current.RemoteCheckpoint = remote.ID
			if er = saveEntry(tx, current); er != nil {
				return er
			}
		}
		if model.IsMarkdownFile(current) && !validPlainResolutionText(r.Content) {
			return model.ErrUnsupportedContent
		}
		if current.Kind == model.KindDocx && int64(len(r.Content)) > LargeSnapshotBytes {
			return model.ErrUnsupportedContent
		}
		if er = supersedeQueued(tx, current.ID); er != nil {
			return er
		}
		e, j, er = s.stageBlob(tx, current.ID, &r.ExpectedGeneration, b)
		if er != nil {
			return er
		}
		if remote != nil {
			rec, err := jobTx(tx, j.ID)
			if err != nil {
				return err
			}
			rec.HumanResolution = true
			if err = saveJob(tx, rec); err != nil {
				return err
			}
		}
		c.Resolved = true
		return put(tx, "conflicts", string(c.ID), c)
	})
	return
}

// SaveDraft keeps recovery material without changing the conflict's captured generation.
func (s *Store) SaveDraft(ctx context.Context, id model.ConflictID, src io.Reader) error {
	b, err := s.PutBlob(ctx, src)
	if err != nil {
		return err
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if er := get(tx, "conflicts", string(id), &c); er != nil {
			return er
		}
		if c.Resolved {
			return errors.New("conflict already resolved")
		}
		c.Draft = b
		return put(tx, "conflicts", string(id), c)
	})
}

func validPlainResolutionText(body []byte) bool {
	return int64(len(body)) <= LargeSnapshotBytes && utf8.Valid(body) && bytes.IndexByte(body, 0) < 0
}
