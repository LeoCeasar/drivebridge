package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func TestHistoryRepeatedConflictRefreshDoesNotCreateVersions(t *testing.T) {
	s, _, _ := openTest(t)
	e, _ := stage(t, s, entry(t, s, "conflict.md"), "draft")
	c := model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Base: e.Local, Local: e.Local, Remote: e.Local, Draft: e.Local, RemoteRevision: "1", Reason: "same_block"}
	if err := s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	before, _ := s.HistoryEvents(ctx, 0, 100)
	for i := 0; i < 3; i++ {
		if err := s.update(ctx, func(tx *bolt.Tx) error {
			current, err := entryTx(tx, e.ID)
			if err != nil {
				return err
			}
			return saveEntry(tx, current)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveDraft(ctx, c.ID, strings.NewReader("draft")); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveConflict(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := s.HistoryEvents(ctx, 0, 100)
	if len(after) != len(before) {
		t.Fatalf("unchanged conflict refresh created %d history versions", len(after)-len(before))
	}
}

func TestHistoryCapturesSavedBytesAndSurvivesRestart(t *testing.T) {
	s, dir, scope := openTest(t)
	e := entry(t, s, "notes.md")
	e, _ = stage(t, s, e, "first\n")
	e, _ = stage(t, s, e, "second\n")
	_, _ = stage(t, s, e, "second\n")
	events, err := s.HistoryEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("repeated unchanged save created history noise: %d events", len(events))
	}
	var saves []model.HistoryEvent
	for _, event := range events {
		if event.Kind == "local_saved" {
			saves = append(saves, event)
		}
	}
	if len(saves) != 2 {
		t.Fatalf("want two content versions, got %#v", saves)
	}
	if got := read(t, s, saves[0].Bodies["local"]); got != "first\n" {
		t.Fatal(got)
	}
	source := saves[0].Source
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
	persisted, err := reopened.HistoryEvents(ctx, 0, 100)
	if err != nil || len(persisted) != len(events) || persisted[0].Source != source {
		t.Fatalf("history restart: %#v %v", persisted, err)
	}
	if err = reopened.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryProtectsUnarchivedBytesFromGC(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "draft.md")
	e, old := stage(t, s, e, "keep this old save")
	for i := 0; i < 12; i++ {
		e, _ = stage(t, s, e, fmt.Sprintf("intermediate-%d", i))
	}
	_, _ = stage(t, s, e, "latest")
	// Completed job retention is independent of the pending history reference.
	if err := s.db.Update(func(tx *bolt.Tx) error {
		r, err := jobTx(tx, old.ID)
		if err != nil {
			return err
		}
		r.CompletedAt = time.Now().Add(-365 * 24 * time.Hour)
		r.Job.State = model.StatusSynced
		return saveJob(tx, r)
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.GC(ctx, GCOptions{Now: time.Now().Add(400 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, old.Local); got != "keep this old save" {
		t.Fatal(got)
	}
}

func TestHistoryMissingAndEmptyBodiesAreDistinct(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "empty.md")
	_, _ = stage(t, s, e, "")
	events, err := s.HistoryEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Bodies["local"] != "" {
		t.Fatal("cold body presented as empty content")
	}
	last := events[len(events)-1]
	if last.Bodies["local"] == "" || read(t, s, last.Bodies["local"]) != "" {
		t.Fatal("empty save lost")
	}
	_, err = s.ReadBlob(ctx, model.BlobID(""))
	if err == nil || errors.Is(err, os.ErrClosed) {
		t.Fatalf("missing blob should fail: %v", err)
	}
}

func TestHistoryRestoreChecksGenerationAndIsIdempotent(t *testing.T) {
	s, _, sc := openTest(t)
	parent, err := s.EnsureEntry(ctx, "", model.KindFolder)
	if err != nil {
		t.Fatal(err)
	}
	e := entry(t, s, "notes.md")
	e, _ = stage(t, s, e, "current")
	req := model.HistoryRestoreRequest{ProfileID: sc.ProfileID, MountID: sc.MountID, EntryID: e.ID, VersionID: "version", RequestID: uuid.NewString(), ExpectedGeneration: e.Generation, ExpectedPath: e.Remote, ParentID: parent.ID, ParentGeneration: parent.Generation}
	result, err := s.RestoreHistory(ctx, req, strings.NewReader("restored"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.LocalSaved || read(t, s, result.Entry.Local) != "restored" {
		t.Fatal(result)
	}
	again, err := s.RestoreHistory(ctx, req, strings.NewReader("restored"), nil)
	if err != nil || again.Entry.Generation != result.Entry.Generation {
		t.Fatalf("duplicate restore %+v %v", again, err)
	}
	req.RequestID = uuid.NewString()
	if _, err = s.RestoreHistory(ctx, req, strings.NewReader("stale"), nil); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatalf("stale accepted %v", err)
	}
}
func TestHistoryRestoreRejectsDeletedPathReoccupied(t *testing.T) {
	s, _, sc := openTest(t)
	parent, err := s.EnsureEntry(ctx, "", model.KindFolder)
	if err != nil {
		t.Fatal(err)
	}
	e := entry(t, s, "old.md")
	e, _ = stage(t, s, e, "old")
	if err = s.update(ctx, func(tx *bolt.Tx) error { return tombstone(tx, e) }); err != nil {
		t.Fatal(err)
	}
	replacement := entry(t, s, "old.md")
	replacement, _ = stage(t, s, replacement, "do not overwrite")
	req := model.HistoryRestoreRequest{ProfileID: sc.ProfileID, MountID: sc.MountID, EntryID: e.ID, VersionID: "version", RequestID: uuid.NewString(), ExpectedGeneration: e.Generation, ExpectedPath: e.Remote, ExpectedDeleted: true, ParentID: parent.ID, ParentGeneration: parent.Generation}
	if _, err = s.RestoreHistory(ctx, req, strings.NewReader("restore old"), nil); !errors.Is(err, ErrDestinationChanged) {
		t.Fatalf("replacement overwritten %v", err)
	}
	got, _ := s.EntryByRemote(ctx, "old.md")
	if got.ID != replacement.ID || read(t, s, got.Local) != "do not overwrite" {
		t.Fatal(got)
	}
}

func TestHistoryCaptureFailureRollsBackSave(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "draft.md")
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("history_events")) }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("must not be accepted")); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("missing outbox accepted save: %v", err)
	}
	current, err := s.Entry(ctx, e.ID)
	if err != nil || current.Local != "" || current.Generation != e.Generation {
		t.Fatalf("failed history capture mutated entry %+v %v", current, err)
	}
}
