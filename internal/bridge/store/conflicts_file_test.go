package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
)

func markdownResolutionFixture(t *testing.T, path string) (*Store, model.Entry, model.Conflict, model.Snapshot) {
	t.Helper()
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, path), "file-token")
	base := fileSnapshot(t, s, e.Token, "base\n")
	var err error
	e, err = s.AdoptFileSnapshot(ctx, e.ID, e.Generation, base.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, _ = stage(t, s, e, "local\n")
	remote := fileSnapshot(t, s, e.Token, "remote\n")
	c := model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Base: base.FileData, Local: e.Local, Remote: remote.FileData, RemoteRevision: "sha256:" + string(remote.FileData), Draft: e.Local}
	if err = s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	return s, e, c, remote
}
func TestResolveAgainstOrdinaryMarkdownAdoptsFileBaseline(t *testing.T) {
	s, e, c, sn := markdownResolutionFixture(t, "note.MD")
	got, j, err := s.ResolveAgainst(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("human merge\n")}, sn)
	if err != nil || got.LocalCheckpoint != sn.FileData || got.RemoteCheckpoint != sn.ID || j.BaseLocal != sn.FileData || j.BaseRemote != sn.ID || read(t, s, j.Local) != "human merge\n" {
		t.Fatal(got, j, err)
	}
	journal, _ := s.Journal(ctx, j.ID)
	if !journal.HumanResolution {
		t.Fatal(journal)
	}
}
func TestResolveAgainstMarkdownRejectsChangedSnapshotAndOtherFileKinds(t *testing.T) {
	for _, mode := range []string{"different hash", "different token", "foreign observed token", "different observed hash", "non markdown", "binary target"} {
		t.Run(mode, func(t *testing.T) {
			name := "note.md"
			if mode == "non markdown" {
				name = "note.txt"
			}
			s, e, c, sn := markdownResolutionFixture(t, name)
			r := model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("human merge")}
			switch mode {
			case "different hash":
				sn.FileData = e.Local
			case "different token":
				sn.Token = "foreign"
			case "foreign observed token":
				sn = fileSnapshot(t, s, "foreign-token", "remote\n")
			case "different observed hash":
				sn = fileSnapshot(t, s, e.Token, "remote changed after review\n")
			case "binary target":
				r.Content = []byte("nul\x00payload")
			}
			_, _, err := s.ResolveAgainst(ctx, r, sn)
			if err == nil {
				t.Fatal("unsafe resolution accepted")
			}
			if mode != "binary target" && !errors.Is(err, model.ErrVersionChanged) {
				t.Fatal(err)
			}
			current, _ := s.Entry(ctx, e.ID)
			if current.LocalCheckpoint != e.LocalCheckpoint || current.RemoteCheckpoint != e.RemoteCheckpoint || current.Generation != e.Generation {
				t.Fatal(current)
			}
			list, _ := s.Conflicts(ctx)
			if len(list) != 1 {
				t.Fatal(list)
			}
		})
	}
}
