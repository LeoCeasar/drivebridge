package store

import (
	"bytes"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateLegacyDirectoryBeforeUpgradeIsReadOnly(t *testing.T) {
	s, dir, scope := openTest(t)
	e, _ := stage(t, s, entry(t, s, "upgrade.md"), "pending local edit")
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range buckets {
			if strings.HasPrefix(name, "history_") {
				if err := tx.DeleteBucket([]byte(name)); err != nil {
					return err
				}
			}
		}
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("1"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "store.db")
	before, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDirectory(ctx, dir, scope); err != nil {
		t.Fatalf("valid schema1 cannot reach installation: %v", err)
	}
	after, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("pre-install audit rewrote schema1: %v", err)
	}
	if err := os.Remove(s.blobPath(e.Local)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDirectory(ctx, dir, scope); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("legacy missing body accepted: %v", err)
	}
}

func TestValidatePreservesUnknownJournal(t *testing.T) {
	s, _, _ := openTest(t)
	_, j := stage(t, s, entry(t, s, "unknown"), "accepted")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnknown(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.Job(ctx, j.ID)
	if err != nil || got.State != model.StatusAwaitingVerification {
		t.Fatal(got, err)
	}
}
func TestValidateRejectsMissingBlob(t *testing.T) {
	s, _, _ := openTest(t)
	e, _ := stage(t, s, entry(t, s, "a"), "accepted")
	if err := os.Remove(s.blobPath(e.Local)); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal(err)
	}
}
func TestValidateRejectsMissingSnapshotReference(t *testing.T) {
	s, _, _ := openTest(t)
	_, j := stage(t, s, entry(t, s, "a"), "accepted")
	if err := s.update(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, j.ID)
		if err != nil {
			return err
		}
		r.Job.BaseRemote = "missing"
		return saveJob(tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal(err)
	}
}
func TestValidateRejectsTamperedBlob(t *testing.T) {
	s, _, _ := openTest(t)
	e, _ := stage(t, s, entry(t, s, "a"), "accepted")
	if err := os.WriteFile(s.blobPath(e.Local), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal(err)
	}
}
func TestValidateDirectoryDoesNotRewriteUnknownState(t *testing.T) {
	s, d, sc := openTest(t)
	_, j := stage(t, s, entry(t, s, "a"), "accepted")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDirectory(ctx, d, sc); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(d+"/store.db", 0600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.View(func(tx *bolt.Tx) error {
		r, err := jobTx(tx, j.ID)
		if err == nil && r.Job.State != model.StatusMerging {
			t.Fatalf("auditor rewrote interrupted state: %+v", r.Job)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
