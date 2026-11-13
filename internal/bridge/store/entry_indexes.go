package store

import (
	"path"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// Membership buckets preserve every binding, including duplicate tokens and
// local-delete tombstones. Visibility is read from metadata at lookup time.
const (
	entryTokens  = "entry_tokens"
	entryParents = "entry_parents"
	entryRemotes = "entry_remotes"
)

func entryParent(e model.Entry) string {
	parent := path.Dir(e.Remote)
	if parent == "." {
		return ""
	}
	return parent
}

func addEntryIndexTx(tx *bolt.Tx, name, key string, id model.EntryID) error {
	b, err := tx.Bucket([]byte(name)).CreateBucketIfNotExists([]byte(key))
	if err != nil {
		return err
	}
	return b.Put([]byte(id), []byte{1})
}

func removeEntryIndexTx(tx *bolt.Tx, name, key string, id model.EntryID) error {
	parent := tx.Bucket([]byte(name))
	b := parent.Bucket([]byte(key))
	if b == nil {
		return nil
	}
	if err := b.Delete([]byte(id)); err != nil {
		return err
	}
	if k, _ := b.Cursor().First(); k == nil {
		return parent.DeleteBucket([]byte(key))
	}
	return nil
}

func updateEntryIndexesTx(tx *bolt.Tx, previous, current model.Entry) error {
	if previous.ID == "" || previous.Token != current.Token {
		if previous.ID != "" && previous.Token != "" {
			if err := removeEntryIndexTx(tx, entryTokens, previous.Token, previous.ID); err != nil {
				return err
			}
		}
		if current.Token != "" {
			if err := addEntryIndexTx(tx, entryTokens, current.Token, current.ID); err != nil {
				return err
			}
		}
	}
	if previous.ID == "" || previous.Remote != current.Remote {
		if previous.ID != "" && previous.Remote != "" {
			if err := removeEntryIndexTx(tx, entryRemotes, previous.Remote, previous.ID); err != nil {
				return err
			}
			if err := removeEntryIndexTx(tx, entryParents, string(pathKey(entryParent(previous))), previous.ID); err != nil {
				return err
			}
		}
		if current.Remote != "" {
			if err := addEntryIndexTx(tx, entryRemotes, current.Remote, current.ID); err != nil {
				return err
			}
			if err := addEntryIndexTx(tx, entryParents, string(pathKey(entryParent(current))), current.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func initEntryIndexesTx(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte("meta"))
	if string(meta.Get([]byte("entry_indexes_version"))) == "2" && tx.Bucket([]byte(entryTokens)) != nil && tx.Bucket([]byte(entryParents)) != nil && tx.Bucket([]byte(entryRemotes)) != nil {
		return nil
	}
	for _, name := range []string{entryTokens, entryParents, entryRemotes} {
		if tx.Bucket([]byte(name)) != nil {
			if err := tx.DeleteBucket([]byte(name)); err != nil {
				return err
			}
		}
		if _, err := tx.CreateBucket([]byte(name)); err != nil {
			return err
		}
	}
	if err := each[model.Entry](tx, "entries", func(e model.Entry) error { return updateEntryIndexesTx(tx, model.Entry{}, e) }); err != nil {
		return err
	}
	return meta.Put([]byte("entry_indexes_version"), []byte("2"))
}

func eachIndexedEntryTx(tx *bolt.Tx, name, key string, fn func(model.Entry) error) error {
	b := tx.Bucket([]byte(name)).Bucket([]byte(key))
	if b == nil {
		return nil
	}
	return b.ForEach(func(id, _ []byte) error {
		e, err := entryTx(tx, model.EntryID(id))
		if err != nil {
			return err
		}
		return fn(e)
	})
}
