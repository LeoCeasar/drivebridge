package syncer

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"testing"
)

func TestSafeSaveBackupCannotDispatchBeforeDurableReplacement(t *testing.T) {
	s, e, cloud, entry := setup(t, model.KindFile)
	operation, err := s.PrepareNamespace(bg, model.NamespaceOp{Kind: "rename", Src: entry.Remote, Dst: entry.Remote + ".sb-1234abcd-Ab12Cd"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureSafeSaveBackup(bg, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(bg, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(bg, operation.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(bg, operation.ID); err != nil {
		t.Fatal(err)
	}
	if err = e.cycle(bg); !errors.Is(err, store.ErrSafeSavePending) {
		t.Fatal("backup rename was not visibly pending", err)
	}
	if cloud.writes != 0 || len(cloud.items) != 0 {
		t.Fatal("pending backup changed cloud", cloud.writes, cloud.items)
	}
	restored, resumed, err := s.ResumeSafeSave(bg, entry.Remote, model.KindFile)
	if err != nil || !resumed {
		t.Fatal(restored, resumed, err)
	}
	if err = e.cycle(bg); !errors.Is(err, store.ErrSafeSavePending) {
		t.Fatal("name alone guessed save completion", err)
	}
	restored = stageContent(t, s, restored, "new complete bytes")
	if err = e.cycle(bg); err != nil {
		t.Fatal(err)
	}
	current, err := s.Entry(bg, entry.ID)
	if err != nil || current.ID != entry.ID || current.Token != entry.Token || current.Status != model.StatusSynced || cloud.writes != 1 || len(cloud.items) != 0 {
		t.Fatalf("replacement did not overwrite same token: %+v writes=%d creates=%+v err=%v", current, cloud.writes, cloud.items, err)
	}
	if cloud.uploadedName != "note.md" || string(cloud.file) != "new complete bytes" {
		t.Fatal(cloud.uploadedName, string(cloud.file))
	}
}
