package store

import (
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

const managedPendingBucket = "managed_pending"

type managedPendingMetadata struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

func liveManagedPending(r managedCheckpointRoute) bool {
	return r.Entry.ID != "" && !r.Retired && !r.HistoryOnly && r.Pending.Sequence > r.LastSequence
}
func managedPendingKey(key string, r managedCheckpointRoute) []byte {
	return append(historyKey(r.Order), []byte(key)...)
}

func updateManagedPendingIndexTx(tx *bolt.Tx, key string, old, current managedCheckpointRoute) error {
	parent := tx.Bucket([]byte(managedPendingBucket))
	if liveManagedPending(old) {
		target := []byte(old.Entry.ID)
		if b := parent.Bucket(target); b != nil {
			if err := b.Delete(managedPendingKey(key, old)); err != nil {
				return err
			}
			if k, _ := b.Cursor().First(); k == nil {
				if err := parent.DeleteBucket(target); err != nil {
					return err
				}
			}
		}
	}
	if !liveManagedPending(current) {
		return nil
	}
	b, err := parent.CreateBucketIfNotExists([]byte(current.Entry.ID))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(managedPendingMetadata{Size: current.Pending.Size, ModTime: current.Pending.ModTime})
	if err != nil {
		return err
	}
	return b.Put(managedPendingKey(key, current), raw)
}

func initManagedPendingIndexTx(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte("meta"))
	if string(meta.Get([]byte("managed_pending_index_version"))) == "1" && tx.Bucket([]byte(managedPendingBucket)) != nil {
		return nil
	}
	if tx.Bucket([]byte(managedPendingBucket)) != nil {
		if err := tx.DeleteBucket([]byte(managedPendingBucket)); err != nil {
			return err
		}
	}
	if _, err := tx.CreateBucket([]byte(managedPendingBucket)); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(managedRoutesBucket)).ForEach(func(key, raw []byte) error {
		var r managedCheckpointRoute
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		return updateManagedPendingIndexTx(tx, string(key), managedCheckpointRoute{}, r)
	}); err != nil {
		return err
	}
	return meta.Put([]byte("managed_pending_index_version"), []byte("1"))
}
