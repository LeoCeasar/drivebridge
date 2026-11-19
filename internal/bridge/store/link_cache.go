package store

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// CacheSourceURL changes metadata only if the queried cloud object still
// occupies the same projected path. Content generations may advance.
func (s *Store) CacheSourceURL(ctx context.Context, expected model.Entry, virtualType, url string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		current, err := entryTx(tx, expected.ID)
		if err != nil {
			return err
		}
		if expected.Token == "" || url == "" || current.MountID != expected.MountID || current.ProfileID != expected.ProfileID || current.Token != expected.Token || current.Kind != expected.Kind || current.Remote != expected.Remote || current.FrozenBy != "" {
			return model.ErrIdentityChanged
		}
		var m Metadata
		if err = get(tx, "metadata", string(current.ID), &m); err != nil {
			return err
		}
		if m.VirtualType != virtualType || m.Tombstone || m.FuseSoftRemove != "" || m.SafeSavePending != "" || m.SafeSaveParked {
			return model.ErrIdentityChanged
		}
		if err = each[model.Conflict](tx, "conflicts", func(c model.Conflict) error {
			if !c.Resolved && c.EntryID == current.ID && c.Reason == model.ConflictRemoteDeleted {
				return model.ErrIdentityChanged
			}
			return nil
		}); err != nil {
			return err
		}
		// Fallback link metadata must not change virtual-file projection content
		// or be erased when a listing omits that projection's source URL.
		m.CachedLinkURL = url
		m.CachedLinkToken = current.Token
		m.CachedLinkKind = string(current.Kind)
		if virtualType != "" {
			m.CachedLinkKind = virtualType
		}
		return put(tx, "metadata", string(current.ID), m)
	})
}
