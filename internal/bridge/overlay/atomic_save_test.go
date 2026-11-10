package overlay

import (
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"testing"
)

func TestTextEditAppleDoubleCleanupRemainsDurableAndLocalOnly(t *testing.T) {
	f, s, _ := fixture(t)
	scratch := "cloud-origin.txt.sb-09847c05-nEhB8i"
	if err := f.Mkdir(ctx, scratch); err != nil {
		t.Fatal(err)
	}
	remote := scratch + "/._cloud-origin.txt"
	put(t, f, remote, "")
	put(t, f, remote, "appledouble metadata one")
	o := put(t, f, remote, "appledouble metadata two")
	entry, err := s.EntryByRemote(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	should, err := f.ShouldSync(ctx, entry.ID)
	if err != nil || should || !f.LocalOnly(remote) {
		t.Fatalf("artifact not isolated: %v %v", should, err)
	}
	if body(t, o) != "appledouble metadata two" {
		t.Fatal("local bytes unavailable")
	}
	if err := o.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 0 {
		t.Fatal(conflicts, err)
	}
	r, err := s.ReadBlob(ctx, entry.Local)
	if err != nil {
		t.Fatal("cleanup lost durable metadata bytes", err)
	}
	r.Close()
	if _, err = s.EntryByRemote(ctx, remote); err != store.ErrNotFound {
		t.Fatalf("metadata cleanup did not remove visible path: %v", err)
	}
}

func TestExactSafeSaveDirectoryAndSubtreeDoNotFilterUserDocuments(t *testing.T) {
	f, s, _ := fixture(t)
	scratch := "cloud-origin.txt.sb-09847c05-nEhB8i"
	if err := f.Mkdir(ctx, scratch); err != nil {
		t.Fatal(err)
	}
	if !f.LocalOnly(scratch) {
		t.Fatal("safe save directory not isolated")
	}
	temp := put(t, f, scratch+"/cloud-origin.txt", "atomic save bytes")
	entry, _ := s.EntryByRemote(ctx, temp.Remote())
	should, err := f.ShouldSync(ctx, entry.ID)
	if err != nil || should {
		t.Fatal(should, err)
	}
	result, err := f.Move(ctx, temp, "saved.txt")
	if err != nil {
		t.Fatal(err)
	}
	entry, _ = s.EntryByRemote(ctx, result.Remote())
	should, err = f.ShouldSync(ctx, entry.ID)
	if err != nil || !should || body(t, result) != "atomic save bytes" {
		t.Fatal("final save did not become syncable", should, err)
	}
	for _, name := range []string{"keep.tmp", "keep.sb-data", "keep.sb-12345678-AbC123", "keep.sb-1234567-AbC123", "keep.sb-12345678-AbC12"} {
		o := put(t, f, name, "user document")
		entry, _ := s.EntryByRemote(ctx, o.Remote())
		should, err := f.ShouldSync(ctx, entry.ID)
		if err != nil || !should || f.LocalOnly(name) {
			t.Fatalf("user file filtered: %s %v %v", name, should, err)
		}
	}
	o := put(t, f, "._saved.txt", "metadata")
	if !f.LocalOnly(o.Remote()) {
		t.Fatal("top-level AppleDouble not isolated")
	}
}

func TestRecognizedOSArtifactDoesNotEraseHistoricalConflict(t *testing.T) {
	f, s, _ := fixture(t)
	put(t, f, "._saved.txt", "historical metadata")
	entry, err := s.EntryByRemote(ctx, "._saved.txt")
	if err != nil {
		t.Fatal(err)
	}
	conflict := model.Conflict{EntryID: entry.ID, Generation: entry.Generation, Local: entry.Local, Reason: "delete_vs_pending_edit"}
	if err := s.SaveConflict(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	should, err := f.ShouldSync(ctx, entry.ID)
	if err != nil || should {
		t.Fatal(should, err)
	}
	conflicts, err := s.Conflicts(ctx)
	if err != nil || len(conflicts) != 1 || conflicts[0].Local != entry.Local || conflicts[0].Reason != conflict.Reason {
		t.Fatalf("history changed: %+v %v", conflicts, err)
	}
	current, err := s.Entry(ctx, entry.ID)
	if err != nil || current.Status == model.StatusSynced {
		t.Fatalf("artifact falsely reported synced: %+v %v", current, err)
	}
	r, err := s.ReadBlob(ctx, entry.Local)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}
