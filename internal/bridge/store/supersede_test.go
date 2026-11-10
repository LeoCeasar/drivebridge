package store

import (
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
)

func TestNewGenerationNeverSupersedesDispatchedUnknownOrCreateJournal(t *testing.T) {
	for _, state := range []string{"merging", "plan", "unknown", "create-journal"} {
		t.Run(state, func(t *testing.T) {
			s, dir, scope := openTest(t)
			current, old := stage(t, s, entry(t, s, "saved.txt"), "old accepted bytes")
			if _, _, err := s.Prepare(ctx, old.ID); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "plan":
				if err := s.RecordPlan(ctx, old.ID, nil, old.Local); err != nil {
					t.Fatal(err)
				}
				if err := s.PauseJob(ctx, old.ID, model.StatusBlocked, "permission_denied"); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if err := s.MarkUnknown(ctx, old.ID); err != nil {
					t.Fatal(err)
				}
			case "create-journal":
				if _, err := s.PrepareCreate(ctx, old.ID, "root", "saved.txt"); err != nil {
					t.Fatal(err)
				}
				if err := s.PauseJob(ctx, old.ID, model.StatusBlocked, "permission_denied"); err != nil {
					t.Fatal(err)
				}
			}
			current, _ = stage(t, s, current, "latest accepted bytes")
			if err := s.SupersedeUnsentPredecessors(ctx, current.ID); err != nil {
				t.Fatal(err)
			}
			check := func(s *Store) {
				t.Helper()
				jobs, err := s.Pending(ctx)
				if err != nil || len(jobs) != 2 || jobs[0].ID != old.ID {
					t.Fatalf("protected predecessor cancelled: %+v %v", jobs, err)
				}
				r, err := s.ReadBlob(ctx, old.Local)
				if err != nil {
					t.Fatal("old accepted bytes unavailable", err)
				}
				r.Close()
			}
			check(s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.BindScope(ctx, scope); err != nil {
				t.Fatal(err)
			}
			if err := reopened.SupersedeUnsentPredecessors(ctx, current.ID); err != nil {
				t.Fatal(err)
			}
			check(reopened)
		})
	}
}
