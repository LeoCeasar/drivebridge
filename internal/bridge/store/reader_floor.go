package store

import (
	"strconv"

	bolt "go.etcd.io/bbolt"
)

// The schema value is a minimum safe reader, not a writer's preferred version.
// Once identity-routed checkpoints exist, a cache-only reader must never replay
// their pending namespace inputs or treat a saved journal base as the full body.
func raiseReaderFloorTx(tx *bolt.Tx, floor int) error {
	meta := tx.Bucket([]byte("meta"))
	raw := string(meta.Get([]byte("schema_version")))
	current := 0
	if raw != "" {
		var err error
		current, err = strconv.Atoi(raw)
		if err != nil {
			return ErrIntegrity
		}
	}
	if current >= floor {
		return nil
	}
	return meta.Put([]byte("schema_version"), []byte(strconv.Itoa(floor)))
}
