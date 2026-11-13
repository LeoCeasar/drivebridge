package store

import (
	"context"
	"errors"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// These durable references bridge process lifetimes while a saved delta still
// needs its original local base. They are released after journal retirement;
// normal archive history and retention continue to be owned by Git.
func managedBasesTx(tx *bolt.Tx) (map[string]model.BlobID, error) {
	refs := map[string]model.BlobID{}
	err := get(tx, "meta", "managed_bases", &refs)
	if errors.Is(err, ErrNotFound) {
		return refs, nil
	}
	if err != nil {
		return nil, err
	}
	if refs == nil {
		return nil, ErrIntegrity
	}
	for key, b := range refs {
		if !canonicalUUID(key) || !blobName.MatchString(string(b)) {
			return nil, ErrIntegrity
		}
	}
	return refs, nil
}

func managedEntriesTx(tx *bolt.Tx) (map[string]model.EntryID, error) {
	refs := map[string]model.EntryID{}
	err := get(tx, "meta", "managed_entries", &refs)
	if errors.Is(err, ErrNotFound) {
		return refs, nil
	}
	if err != nil {
		return nil, err
	}
	if refs == nil {
		return nil, ErrIntegrity
	}
	for key, id := range refs {
		if !canonicalUUID(key) || !canonicalUUID(string(id)) {
			return nil, ErrIntegrity
		}
	}
	return refs, nil
}

func managedEntryPending(tx *bolt.Tx, id model.EntryID) bool {
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return true
	}
	for _, entry := range refs {
		if entry == id {
			return true
		}
	}
	return false
}

// Protect the entry even when a new file has no base blob yet. Remote reads or
// deletion observations cannot advance its baseline before WAL acceptance has
// reached the immutable outbox. Namespace operations retain their own ordering.
func (s *Store) RetainManagedEntry(ctx context.Context, key string, id model.EntryID, b model.BlobID) error {
	if !canonicalUUID(key) || !canonicalUUID(string(id)) || b != "" && !blobName.MatchString(string(b)) {
		return ErrIntegrity
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		e, err := entryTx(tx, id)
		if err != nil {
			return err
		}
		if e.Visible != b {
			return model.ErrGenerationChanged
		}
		if err = s.checkBlob(b); err != nil {
			return err
		}
		bases, err := managedBasesTx(tx)
		if err != nil {
			return err
		}
		entries, err := managedEntriesTx(tx)
		if err != nil {
			return err
		}
		if old := entries[key]; old != "" && old != id {
			return model.ErrIdentityChanged
		}
		if b != "" {
			bases[key] = b
		}
		entries[key] = id
		// Old applications understand mutable cache files only. Advancing the
		// reader floor before the first WAL write prevents an older binary from
		// staging the sparse base as if it contained the saved journal bytes.
		if err = tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("3")); err != nil {
			return err
		}
		if err = put(tx, "meta", "managed_bases", bases); err != nil {
			return err
		}
		return put(tx, "meta", "managed_entries", entries)
	})
}

func (s *Store) RetainManagedBase(ctx context.Context, key string, b model.BlobID) error {
	if !canonicalUUID(key) || !blobName.MatchString(string(b)) {
		return ErrIntegrity
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		if err := s.checkBlob(b); err != nil {
			return err
		}
		refs, err := managedBasesTx(tx)
		if err != nil {
			return err
		}
		if old := refs[key]; old != "" && old != b {
			return model.ErrIdentityChanged
		}
		refs[key] = b
		if err = tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("3")); err != nil {
			return err
		}
		return put(tx, "meta", "managed_bases", refs)
	})
}
func (s *Store) ReleaseManagedBase(ctx context.Context, key string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		refs, err := managedBasesTx(tx)
		if err != nil {
			return err
		}
		delete(refs, key)
		if err = put(tx, "meta", "managed_bases", refs); err != nil {
			return err
		}
		entries, err := managedEntriesTx(tx)
		if err != nil {
			return err
		}
		delete(entries, key)
		return put(tx, "meta", "managed_entries", entries)
	})
}
func (s *Store) ReconcileManagedBases(ctx context.Context, live map[string]bool) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		refs, err := managedBasesTx(tx)
		if err != nil {
			return err
		}
		for key := range refs {
			if !live[key] {
				delete(refs, key)
			}
		}
		if err = put(tx, "meta", "managed_bases", refs); err != nil {
			return err
		}
		entries, err := managedEntriesTx(tx)
		if err != nil {
			return err
		}
		for key := range entries {
			if !live[key] {
				delete(entries, key)
			}
		}
		return put(tx, "meta", "managed_entries", entries)
	})
}
