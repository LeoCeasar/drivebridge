package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"testing"
)

func TestEmptyThenNonemptyBeforeEngineSyncsLatest(t *testing.T) {
	for _, localNew := range []bool{false, true} {
		name := "existing-file"
		if localNew {
			name = "new-file"
		}
		t.Run(name, func(t *testing.T) {
			s, e, source, entry := setup(t, model.KindFile)
			if localNew {
				var err error
				entry, err = s.EnsureEntry(bg, "saved.txt", model.KindFile)
				if err != nil {
					t.Fatal(err)
				}
			}
			entry = stageContent(t, s, entry, "")
			old, _ := s.Pending(bg)
			entry = stageContent(t, s, entry, "complete saved bytes")
			if err := e.SyncEntry(bg, entry.ID); err != nil {
				t.Fatal(err)
			}
			jobs, err := s.Pending(bg)
			if err != nil || len(jobs) != 0 || source.writes != 1 {
				t.Fatalf("jobs=%+v writes=%d err=%v", jobs, source.writes, err)
			}
			current, _ := s.Entry(bg, entry.ID)
			if current.Status != model.StatusSynced || blobText(t, s, current.Local) != "complete saved bytes" {
				t.Fatal(current)
			}
			history, err := s.Job(bg, old[0].ID)
			if err != nil || history.State == model.StatusSynced || blobText(t, s, history.Local) != "" {
				t.Fatalf("history lost or falsely confirmed: %+v %v", history, err)
			}
		})
	}
}

func TestEmptyBlockedBeforeNonemptyControl(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "")
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "complete saved bytes")
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Status != model.StatusSynced || source.writes != 1 {
		t.Fatalf("control failed: %+v writes=%d", current, source.writes)
	}
}

func TestLegacyBlockedEmptyGenerationRecoversAfterRestart(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "")
	jobs, _ := s.Pending(bg)
	old := jobs[0]
	if _, _, err := s.Prepare(bg, old.ID); err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "latest after save")
	if err := s.PauseJob(bg, old.ID, model.StatusBlocked, "empty_file_unsupported"); err != nil {
		t.Fatal(err)
	}
	scope, err := s.Scope(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Open(source.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err = recovered.BindScope(bg, scope); err != nil {
		t.Fatal(err)
	}
	e, err = New(Dependencies{Store: recovered, Source: source, Publisher: PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := recovered.CommitPublication(ctx, c)
		return err
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := recovered.Entry(bg, entry.ID)
	pending, _ := recovered.Pending(bg)
	if current.Status != model.StatusSynced || len(pending) != 0 || source.writes != 1 {
		t.Fatalf("current=%+v pending=%+v writes=%d", current, pending, source.writes)
	}
	history, err := recovered.Job(bg, old.ID)
	if err != nil || history.State != model.StatusBlocked || blobText(t, recovered, history.Local) != "" {
		t.Fatalf("old record not retained: %+v %v", history, err)
	}
}
