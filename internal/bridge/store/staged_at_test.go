package store

import (
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestAtomicReplacementRecordsFreshStagingTime(t *testing.T) {
	s, _, _ := openTest(t)
	destination := bind(t, s, entry(t, s, "saved.txt"), "remote-token")
	source, _ := stage(t, s, entry(t, s, "temporary"), "complete replacement")
	operation, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: source.Remote, Dst: destination.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(ctx, operation.ID); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].EntryID != destination.ID {
		t.Fatal(jobs, err)
	}
	journal, err := s.Journal(ctx, jobs[0].ID)
	if err != nil || journal.StagedAt.IsZero() {
		t.Fatal("atomic replacement bypasses write-back clock", journal, err)
	}
}
