package store

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
	"time"
)

// ClaimConflictNotification durably reserves the first desktop notification for
// one existing conflict. Call only after checking an interactive desktop exists.
func (s *Store) ClaimConflictNotification(ctx context.Context, id model.ConflictID) (claimed bool, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var c model.Conflict
		if er := get(tx, "conflicts", string(id), &c); er != nil {
			return er
		}
		if c.Resolved {
			return nil
		}
		key := "notification:" + string(id)
		var at time.Time
		if er := get(tx, "meta", key, &at); er == nil {
			return nil
		} else if !errors.Is(er, ErrNotFound) {
			return er
		}
		if er := put(tx, "meta", key, time.Now().UTC()); er != nil {
			return er
		}
		claimed = true
		return nil
	})
	return
}
