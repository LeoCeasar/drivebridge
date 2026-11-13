package syncer

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"strings"
	"testing"
)

func legacyFormatFixture(t *testing.T, body string) (*store.Store, *Engine, *fixtureCloud, model.Entry, model.Job, model.Conflict) {
	t.Helper()
	s, e, source, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, body)
	jobs, err := s.Pending(bg)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	job, en, err := s.Prepare(bg, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	sn, err := s.Snapshot(bg, en.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.conflict(bg, job, sn, "unsupported_content"); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	cs, err := s.Conflicts(bg)
	if err != nil || len(cs) != 1 {
		t.Fatal(cs, err)
	}
	return s, e, source, en, job, cs[0]
}
func TestFormatFailureIsBlockedInsteadOfManualMerge(t *testing.T) {
	s, e, source, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, "alpha one\n\n<span>not supported</span>\n\nbeta two\n")
	if err := e.SyncEntry(bg, en.ID); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatalf("format failure = %v", err)
	}
	cs, err := s.Conflicts(bg)
	if err != nil || len(cs) != 0 {
		t.Fatal("format failure requested text merge", cs, err)
	}
	jobs, err := s.Pending(bg)
	if err != nil || len(jobs) != 1 || jobs[0].State != model.StatusBlocked {
		t.Fatal(jobs, err)
	}
	journal, err := s.Journal(bg, jobs[0].ID)
	if err != nil || journal.ErrorCode != "unsupported_content" || source.writes != 0 {
		t.Fatal(journal, source.writes, err)
	}
	got, _ := s.Entry(bg, en.ID)
	if got.Local != en.Local {
		t.Fatal("local content changed")
	}
}
func TestLegacyFormatConflictRetriesWithCurrentPlanner(t *testing.T) {
	s, e, source, en, job, _ := legacyFormatFixture(t, "alpha one\n\n[guide](skills/example.md)\n\nbeta two\n")
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(bg, en.ID)
	cs, _ := s.Conflicts(bg)
	if got.Status != model.StatusSynced || len(cs) != 0 || source.writes == 0 {
		t.Fatalf("legacy format remains blocked: status=%s conflicts=%d writes=%d", got.Status, len(cs), source.writes)
	}
	journal, err := s.Journal(bg, job.ID)
	if err != nil || journal.HumanResolution {
		t.Fatal("automatic retry acquired human authority", journal, err)
	}
}
func TestLegacyFormatRetryPreservesEditedHumanDraft(t *testing.T) {
	s, e, source, en, _, c := legacyFormatFixture(t, "alpha local\n\nbeta two\n")
	if err := s.SaveDraft(bg, c.ID, strings.NewReader("separately edited human draft")); err != nil {
		t.Fatal(err)
	}
	if err := e.Retry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.Conflicts(bg)
	if len(cs) != 1 || source.writes != 0 || blobText(t, s, cs[0].Draft) != "separately edited human draft" {
		t.Fatal("edited draft consumed", cs, source.writes)
	}
}
func TestLegacyFormatRetryRechecksCloudChanges(t *testing.T) {
	s, e, source, en, _, _ := legacyFormatFixture(t, "alpha local\n\nbeta two\n")
	source.xml = strings.Replace(source.xml, "alpha one", "alpha remote", 1)
	source.revision++
	if err := e.Retry(bg, en.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal("remote change ignored", err)
	}
	cs, _ := s.Conflicts(bg)
	if len(cs) != 1 || cs[0].Reason == "unsupported_content" || source.writes != 0 {
		t.Fatal("true conflict overwritten", cs, source.writes)
	}
}
