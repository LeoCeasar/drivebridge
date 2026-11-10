package store

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"strings"
	"testing"
)

func TestLocalReceiptIsDurableAndNeverClaimsCloudConfirmation(t *testing.T) {
	s, dir, scope := openTest(t)
	e := entry(t, s, ".DS_Store")
	e, job, err := s.StageLocalOnly(ctx, e.ID, nil, strings.NewReader("Finder metadata"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := s.Journal(ctx, job.ID)
	if err != nil || !journal.LocalReceipt || journal.Job.State == model.StatusSynced {
		t.Fatal(journal, err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("receipt allowed cloud claim", err)
	}
	if err = s.RecordPlan(ctx, job.ID, nil, job.Local); !errors.Is(err, ErrNotFound) {
		t.Fatal("receipt allowed plan", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
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
	if err = reopened.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	journal, err = reopened.Journal(ctx, job.ID)
	if err != nil || !journal.LocalReceipt || read(t, reopened, job.Local) != "Finder metadata" {
		t.Fatal(journal, err)
	}
	if ok, err := reopened.CancelUnsentLocalFile(ctx, e.ID, e.Generation); err != nil || !ok {
		t.Fatal("local receipt cannot be removed safely", ok, err)
	}
	if read(t, reopened, job.Local) != "Finder metadata" {
		t.Fatal("removing receipt lost bytes")
	}
}

func TestUnsafeHistoryRefusesLocalReceiptAndRemainsVisible(t *testing.T) {
	for _, state := range []string{"planned", "unknown", "create-journal", "conflict"} {
		t.Run(state, func(t *testing.T) {
			s, _, _ := openTest(t)
			e, old := stage(t, s, entry(t, s, ".DS_Store"), "historical bytes")
			if state == "conflict" {
				if err := s.SaveConflict(ctx, model.Conflict{EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "historical conflict"}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, _, err := s.Prepare(ctx, old.ID); err != nil {
					t.Fatal(err)
				}
				switch state {
				case "planned":
					if err := s.RecordPlan(ctx, old.ID, nil, old.Local); err != nil {
						t.Fatal(err)
					}
				case "unknown":
					if err := s.MarkUnknown(ctx, old.ID); err != nil {
						t.Fatal(err)
					}
				case "create-journal":
					if _, err := s.PrepareCreate(ctx, old.ID, "root", ".DS_Store"); err != nil {
						t.Fatal(err)
					}
					if err := s.PauseJob(ctx, old.ID, model.StatusBlocked, "permission_denied"); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, latest, err := s.StageLocalOnly(ctx, e.ID, nil, strings.NewReader("new local bytes"))
			if err != nil {
				t.Fatal(err)
			}
			journal, err := s.Journal(ctx, latest.ID)
			if err != nil || journal.LocalReceipt {
				t.Fatal("unsafe history hidden by receipt", journal, err)
			}
			pending, err := s.Pending(ctx)
			if err != nil || len(pending) != 2 || pending[0].ID != old.ID {
				t.Fatal("protected cloud work disappeared", pending, err)
			}
			if read(t, s, old.Local) != "historical bytes" {
				t.Fatal("historical bytes lost")
			}
			if state == "conflict" {
				conflicts, err := s.Conflicts(ctx)
				if err != nil || len(conflicts) != 1 {
					t.Fatal(conflicts, err)
				}
			}
		})
	}
}

func TestLegacyJobWithoutReceiptMarkerStaysInOutboxOnReopen(t *testing.T) {
	s, dir, scope := openTest(t)
	_, job := stage(t, s, entry(t, s, ".DS_Store"), "legacy accepted bytes")
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
	if err = reopened.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := reopened.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != job.ID {
		t.Fatal("legacy work was silently hidden", pending, err)
	}
	journal, err := reopened.Journal(ctx, job.ID)
	if err != nil || journal.LocalReceipt {
		t.Fatal(journal, err)
	}
}
