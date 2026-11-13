package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// Model the released reader's schema guard, before its legacy cache replay.
func replayWithSchema3Reader(dir string, replay func() error) error {
	db, err := bolt.Open(filepath.Join(dir, "store.db"), 0600, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.View(func(tx *bolt.Tx) error {
		version := string(tx.Bucket([]byte("meta")).Get([]byte("schema_version")))
		if version != "1" && version != "2" && version != "3" {
			return fmt.Errorf("unsupported store schema %q", version)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return replay()
}

func TestManagedIdentityReaderFloorRejectsOldCacheReplay(t *testing.T) {
	s, dir, scope := openTest(t)
	e := entry(t, s, "saved.tmp")
	key := retainCheckpoint(t, s, e)
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		require.Equal(t, "4", string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))))
		return nil
	}))
	require.NoError(t, s.Close())
	wal := filepath.Join(dir, "accepted-save.wal")
	require.NoError(t, os.WriteFile(wal, []byte("already acknowledged local bytes"), 0600))
	err := replayWithSchema3Reader(dir, func() error { return os.Remove(wal) })
	require.ErrorContains(t, err, "unsupported store schema")
	content, err := os.ReadFile(wal)
	require.NoError(t, err)
	require.Equal(t, "already acknowledged local bytes", string(content))
	s, err = Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.BindScope(ctx, scope))
	require.NoError(t, s.ReleaseManagedBase(ctx, key))
	// An older base-only retention operation must not lower the reader floor.
	b, err := s.PutBlob(ctx, strings.NewReader("pinned base"))
	require.NoError(t, err)
	require.NoError(t, s.RetainManagedBase(ctx, uuid.NewString(), b))
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		require.Equal(t, "4", string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))))
		return nil
	}))
	require.NoError(t, s.Validate(ctx))
}

func TestLegacySchema3RemainsReadableAndUpgradesOnIdentityRoute(t *testing.T) {
	s, dir, scope := openTest(t)
	e := entry(t, s, "legacy")
	require.NoError(t, s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("3")) }))
	require.NoError(t, s.Close())
	s, err := Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.BindScope(ctx, scope))
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		require.Equal(t, "3", string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))))
		return nil
	}))
	retainCheckpoint(t, s, e)
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		require.Equal(t, "4", string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))))
		return nil
	}))
	require.NoError(t, s.Validate(ctx))
}
