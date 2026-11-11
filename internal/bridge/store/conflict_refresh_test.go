package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestSaveConflictRefreshPreservesNewDraft(t *testing.T) {
	s, _, _ := openTest(t)
	c := copyConflict(t, s)
	list, err := s.Conflicts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	c = list[0]
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("newer user draft")); err != nil {
		t.Fatal(err)
	}
	c.Remote, err = s.PutBlob(ctx, strings.NewReader("newer cloud input"))
	if err != nil {
		t.Fatal(err)
	}
	c.RemoteRevision = "newer-revision"
	if err = s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	list, err = s.Conflicts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if read(t, s, list[0].Draft) != "newer user draft" || list[0].Remote != c.Remote || list[0].RemoteRevision != c.RemoteRevision {
		t.Fatal("refresh lost draft or cloud inputs", list[0])
	}
}

func TestSaveConflictRefreshCannotReviveResolvedConflict(t *testing.T) {
	s, _, _ := openTest(t)
	c := copyConflict(t, s)
	resolved, job, err := s.Resolve(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("resolved content")})
	if err != nil {
		t.Fatal(err)
	}
	// RefreshConflict updates its candidate's generation after the cloud fetch.
	// A generation check alone therefore cannot reject a concurrent resolution.
	c.Generation, c.Local = resolved.Generation, resolved.Local
	if err = s.SaveConflict(ctx, c); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stale refresh revived resolved conflict: %v", err)
	}
	list, err := s.Conflicts(ctx)
	if err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	current, err := s.Entry(ctx, c.EntryID)
	if err != nil || current != resolved {
		t.Fatal("failed refresh changed resolved entry", current, err)
	}
	currentJob, err := s.Job(ctx, job.ID)
	if err != nil || currentJob.State != model.StatusQueued {
		t.Fatal("failed refresh changed resolution job", currentJob, err)
	}
}

func TestSaveConflictRefreshKeepsIdentityAndGenerationChecks(t *testing.T) {
	s, _, _ := openTest(t)
	c := copyConflict(t, s)
	other, _ := stage(t, s, entry(t, s, "other"), "other content")
	changed := c
	changed.EntryID, changed.Generation, changed.Local = other.ID, other.Generation, other.Local
	if err := s.SaveConflict(ctx, changed); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("conflict identity changed: %v", err)
	}
	changed = c
	changed.Generation--
	if err := s.SaveConflict(ctx, changed); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatalf("stale generation accepted: %v", err)
	}
}

func TestSaveConflictCannotResolveViaRefresh(t *testing.T) {
	s, _, _ := openTest(t)
	c := copyConflict(t, s)
	c.Resolved = true
	if err := s.SaveConflict(ctx, c); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("refresh bypassed explicit resolution: %v", err)
	}
	list, err := s.Conflicts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
}
