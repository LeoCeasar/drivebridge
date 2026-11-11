package syncer

import (
	"context"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestNamespaceSyncPolicyRetainsDisabledActions(t *testing.T) {
	for _, kind := range []string{"delete", "rename"} {
		t.Run(kind, func(t *testing.T) {
			s, e, c, en := setup(t, model.KindFile)
			c.items = []larkcli.DriveItem{{Token: en.Token, Name: en.CloudName, Type: "file", ParentToken: "root"}}
			action := model.NamespaceOp{Kind: kind, Src: en.Remote}
			if kind == "rename" {
				action.Dst = "visible.md"
			}
			op, err := s.PrepareNamespace(bg, action)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.SealNamespace(bg, op.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.CommitNamespace(bg, op.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.FinishNamespace(bg, op.ID); err != nil {
				t.Fatal(err)
			}
			enabled := false
			e.deps.ShouldSyncPath = func(context.Context, string) (bool, error) { return enabled, nil }
			if err = e.syncNamespace(bg); err != nil {
				t.Fatal(err)
			}
			pending, _ := s.NamespaceActions(bg)
			if len(pending) != 1 || c.deletes != 0 || c.writes != 0 || c.items[0].Name != en.CloudName {
				t.Fatal("disabled namespace dispatched or lost", pending, c.deletes, c.items)
			}
			journal, err := s.NamespaceJournal(bg, op.ID)
			if err != nil || journal.Dispatched {
				t.Fatal(journal, err)
			}
			enabled = true
			if err = e.syncNamespace(bg); err != nil {
				t.Fatal(err)
			}
			pending, _ = s.NamespaceActions(bg)
			if len(pending) != 0 {
				t.Fatal("namespace did not resume", pending)
			}
			if kind == "delete" && c.deletes != 1 {
				t.Fatal(c.deletes)
			}
			if kind == "rename" && c.items[0].Name != "visible.md" {
				t.Fatal(c.items)
			}
		})
	}
}
