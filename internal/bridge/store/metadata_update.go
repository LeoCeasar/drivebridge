package store

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// RecordCloudName updates observed cloud metadata by stable identity. A local
// rename may already have moved the path again while the remote RPC was running.
func (s *Store) RecordCloudName(ctx context.Context, id model.EntryID, token, name string) error {
	if token == "" || name == "" {
		return errors.New("cloud name requires a bound resource")
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		en, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		if en.Token != token {
			return model.ErrIdentityChanged
		}
		en.CloudName = name
		return saveEntry(tx, en)
	})
}
