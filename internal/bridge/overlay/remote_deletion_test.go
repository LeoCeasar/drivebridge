package overlay

import (
	"errors"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestCompleteDirectoryRefreshRemovesCleanMissingFile(t *testing.T) {
	f, st, source := fixture(t)
	source.items = []larkcli.DriveItem{{Token: "t", Name: "note.md", Type: "file"}}
	source.bodies["t"] = "clean"
	listed(t, f)
	source.items = nil
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EntryByRemote(ctx, "note.md"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("missing file stayed visible", err)
	}
}

func TestCompleteDirectoryRefreshProtectsPendingFileAndDoesNotReadBodies(t *testing.T) {
	f, st, source := fixture(t)
	source.items = []larkcli.DriveItem{{Token: "t", Name: "note.md", Type: "file"}}
	source.bodies["t"] = "base"
	listed(t, f)
	put(t, f, "note.md", "local changed")
	before := source.observes
	source.items = nil
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	conflicts, err := st.Conflicts(ctx)
	if err != nil || len(conflicts) != 1 || conflicts[0].Reason != model.ConflictRemoteDeleted {
		t.Fatal(conflicts, err)
	}
	if source.observes != before {
		t.Fatal("metadata scan downloaded a body")
	}
	if _, err := st.EntryByRemote(ctx, "note.md"); err != nil {
		t.Fatal(err)
	}
	source.listErr = errors.New("offline")
	if err := f.RefreshDirectory(ctx, ""); err == nil {
		t.Fatal("failed listing accepted as absence")
	}
	conflicts, _ = st.Conflicts(ctx)
	if len(conflicts) != 1 {
		t.Fatal("repeat refresh duplicated conflict", conflicts)
	}
}

func TestRemoteCleanDeletionDoesNotPermanentlyHideReappearedFile(t *testing.T) {
	f, st, source := fixture(t)
	items := []larkcli.DriveItem{{Token: "returning-token", Name: "note.md", Type: "file"}}
	source.items = items
	source.bodies["returning-token"] = "before deletion"
	listed(t, f)
	prior, err := st.EntryByToken(ctx, "returning-token")
	if err != nil {
		t.Fatal(err)
	}
	source.items = nil
	if err = f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	source.items = items
	source.bodies["returning-token"] = "restored and changed content"
	if err = f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	current, err := st.EntryByRemote(ctx, "note.md")
	if err != nil || current.ID == prior.ID || current.Token != "returning-token" {
		t.Fatal(current, err)
	}
	o, err := f.NewObject(ctx, "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := body(t, o); got != "restored and changed content" {
		t.Fatal(got)
	}
}
