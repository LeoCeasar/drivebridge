package store

import (
	"errors"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// A point lookup must not deserialize unrelated catalog rows. Besides avoiding
// catalog-sized work, this keeps damage in an unrelated row out of the lookup.
func TestCatalogLookupsReadOnlyMatchingEntries(t *testing.T) {
	s, _, _ := openTest(t)
	wanted := bind(t, s, entry(t, s, "folder/wanted"), "wanted-token")
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("entries")).Put([]byte("unrelated"), []byte("invalid json"))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.EntryByToken(ctx, "wanted-token")
	if err != nil || got.ID != wanted.ID {
		t.Fatalf("token lookup: %+v, %v", got, err)
	}
	children, err := s.Entries(ctx, "folder")
	if err != nil || len(children) != 1 || children[0].ID != wanted.ID {
		t.Fatalf("parent lookup: %+v, %v", children, err)
	}
}

func TestCatalogLookupsTrackMutationAndTombstones(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "before/item"), "old-token")
	if err := s.update(ctx, func(tx *bolt.Tx) error {
		updated := original
		updated.Remote, updated.Token = "after/item", "new-token"
		return saveEntry(tx, updated)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EntryByToken(ctx, "old-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token: %v", err)
	}
	got, err := s.EntryByToken(ctx, "new-token")
	if err != nil || got.Remote != "after/item" {
		t.Fatalf("new token: %+v %v", got, err)
	}
	children, err := s.Entries(ctx, "before")
	if err != nil || len(children) != 0 {
		t.Fatalf("old parent: %+v %v", children, err)
	}
	children, err = s.Entries(ctx, "after")
	if err != nil || len(children) != 1 || children[0].ID != original.ID {
		t.Fatalf("new parent: %+v %v", children, err)
	}
	if err = s.update(ctx, func(tx *bolt.Tx) error {
		var metadata Metadata
		if err := get(tx, "metadata", string(original.ID), &metadata); err != nil {
			return err
		}
		metadata.Tombstone = true
		return put(tx, "metadata", string(original.ID), metadata)
	}); err != nil {
		t.Fatal(err)
	}
	children, err = s.Entries(ctx, "after")
	if err != nil || len(children) != 0 {
		t.Fatalf("tombstone children: %+v %v", children, err)
	}
	if _, err = s.EntryByToken(ctx, "new-token"); err != nil {
		t.Fatalf("local deletion loses binding: %v", err)
	}
	if err = s.update(ctx, func(tx *bolt.Tx) error {
		e, err := entryTx(tx, original.ID)
		if err != nil {
			return err
		}
		e.Token = ""
		return saveEntry(tx, e)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EntryByToken(ctx, "new-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remote deletion keeps binding: %v", err)
	}
}

func TestCatalogIndexMigrationPreservesDuplicateTokenIdentities(t *testing.T) {
	s, dir, scope := openTest(t)
	first := bind(t, s, entry(t, s, "one"), "duplicate")
	second := bind(t, s, entry(t, s, "two"), "duplicate")
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"entry_tokens", "entry_parents"} {
			if err := tx.DeleteBucket([]byte(name)); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
				return err
			}
		}
		return tx.Bucket([]byte("meta")).Delete([]byte("entry_indexes_version"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.EntryByToken(ctx, "duplicate"); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("duplicate binding: %v", err)
	}
	children, err := reopened.Entries(ctx, "")
	if err != nil || len(children) != 2 {
		t.Fatalf("migrated children: %+v %v", children, err)
	}
	if err = reopened.update(ctx, func(tx *bolt.Tx) error {
		e, err := entryTx(tx, second.ID)
		if err != nil {
			return err
		}
		e.Token = "replacement"
		return saveEntry(tx, e)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.EntryByToken(ctx, "duplicate")
	if err != nil || got.ID != first.ID {
		t.Fatalf("remaining binding: %+v %v", got, err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	got, err = reopened.EntryByToken(ctx, "replacement")
	if err != nil || got.ID != second.ID {
		t.Fatalf("persisted binding: %+v %v", got, err)
	}
	children, err = reopened.Entries(ctx, "")
	if err != nil || len(children) != 2 {
		t.Fatalf("persisted children: %+v %v", children, err)
	}
}

func TestCatalogIndexesRollBackWithEntryWrite(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "before/item"), "old-token")
	aborted := errors.New("abort")
	if err := s.update(ctx, func(tx *bolt.Tx) error {
		updated := original
		updated.Remote, updated.Token = "after/item", "new-token"
		if err := saveEntry(tx, updated); err != nil {
			return err
		}
		return aborted
	}); !errors.Is(err, aborted) {
		t.Fatal(err)
	}
	got, err := s.EntryByToken(ctx, "old-token")
	if err != nil || got.ID != original.ID {
		t.Fatalf("rolled-back token: %+v %v", got, err)
	}
	if _, err = s.EntryByToken(ctx, "new-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aborted token retained: %v", err)
	}
	children, err := s.Entries(ctx, "before")
	if err != nil || len(children) != 1 || children[0].ID != original.ID {
		t.Fatalf("rolled-back parent: %+v %v", children, err)
	}
	children, err = s.Entries(ctx, "after")
	if err != nil || len(children) != 0 {
		t.Fatalf("aborted parent retained: %+v %v", children, err)
	}
}
