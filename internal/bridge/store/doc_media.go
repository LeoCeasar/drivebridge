package store

import (
	"context"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// CachedDocMedia returns revision-bound references only. Callers still acquire
// ReadBlob leases before reuse; a missing/corrupt cache is never an empty image.
func (s *Store) CachedDocMedia(ctx context.Context, token, revision string) (map[string]model.BlobID, error) {
	result := map[string]model.BlobID{}
	newest := map[string]time.Time{}
	err := s.view(ctx, func(tx *bolt.Tx) error {
		return each[snapshotRecord](tx, "snapshots", func(r snapshotRecord) error {
			if r.Snapshot.Token != token || r.Snapshot.Revision != revision {
				return nil
			}
			for name, b := range r.Snapshot.DocMedia {
				if r.SavedAt.After(newest[name]) {
					result[name] = b
					newest[name] = r.SavedAt
				}
			}
			return nil
		})
	})
	return result, err
}

// CachedDocuments selects only already-materialized bodies. An upgrade must not
// hydrate cold documents or enumerate additional cloud folders.
func (s *Store) CachedDocuments(ctx context.Context) (out []model.Entry, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[model.Entry](tx, "entries", func(en model.Entry) error {
			if en.Kind != model.KindDocx || en.Visible == "" || en.RemoteCheckpoint == "" || en.FrozenBy != "" {
				return nil
			}
			var m Metadata
			if err := get(tx, "metadata", string(en.ID), &m); err != nil {
				return err
			}
			if !m.Tombstone {
				out = append(out, en)
			}
			return nil
		})
	})
	return
}
