package store

import (
	"encoding/json"

	bolt "go.etcd.io/bbolt"
)

const namespaceActiveIndex = "namespace_active"
const namespaceActionIndex = "namespace_actions"

func indexNamespaceTx(tx *bolt.Tx, r namespaceRecord) error {
	for name, live := range map[string]bool{
		namespaceActiveIndex: r.Op.State == "prepared" || r.Op.State == "sealed" || r.Op.State == "committed",
		namespaceActionIndex: r.ActionPending,
	} {
		b := tx.Bucket([]byte(name))
		if live {
			if err := b.Put([]byte(r.Op.ID), []byte{1}); err != nil {
				return err
			}
		} else if err := b.Delete([]byte(r.Op.ID)); err != nil {
			return err
		}
	}
	return nil
}

func initNamespaceIndexesTx(tx *bolt.Tx) error {
	meta := tx.Bucket([]byte("meta"))
	if string(meta.Get([]byte("namespace_indexes_version"))) == "1" && tx.Bucket([]byte(namespaceActiveIndex)) != nil && tx.Bucket([]byte(namespaceActionIndex)) != nil {
		return nil
	}
	for _, name := range []string{namespaceActiveIndex, namespaceActionIndex} {
		if tx.Bucket([]byte(name)) != nil {
			if err := tx.DeleteBucket([]byte(name)); err != nil {
				return err
			}
		}
		if _, err := tx.CreateBucket([]byte(name)); err != nil {
			return err
		}
	}
	if err := each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error { return indexNamespaceTx(tx, r) }); err != nil {
		return err
	}
	return meta.Put([]byte("namespace_indexes_version"), []byte("1"))
}

func eachIndexedNamespaceTx(tx *bolt.Tx, name string, fn func(namespaceRecord) error) error {
	var records []namespaceRecord
	if err := tx.Bucket([]byte(name)).ForEach(func(key, _ []byte) error {
		var r namespaceRecord
		raw := tx.Bucket([]byte("namespace")).Get(key)
		if raw == nil {
			return ErrIntegrity
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		records = append(records, r)
		return nil
	}); err != nil {
		return err
	}
	for _, r := range records {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}
