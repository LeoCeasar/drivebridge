package store

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
	"os"
	"testing"
)

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
