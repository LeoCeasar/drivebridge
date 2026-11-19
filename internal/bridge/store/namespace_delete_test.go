package store

import (
	"errors"
	bolt "go.etcd.io/bbolt"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestConfirmedDeleteAbsenceRequiresCapturedIdentityAndParent(t *testing.T) {
	for _, variant := range []string{"valid", "entry generation", "entry token", "parent token", "parent generation", "wrong parent", "wrong operation"} {
		t.Run(variant, func(t *testing.T) {
			s, _, _ := openTest(t)
			parent, err := s.ObserveEntry(ctx, Observation{Remote: "parent", Kind: model.KindFolder, Token: "parent-token"})
			if err != nil {
				t.Fatal(err)
			}
			e, err := s.ObserveEntry(ctx, Observation{Remote: "parent/file", Kind: model.KindFile, Token: "original"})
			if err != nil {
				t.Fatal(err)
			}
			op := deletionNamespace(t, s, e)
			if err = s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
				t.Fatal(err)
			}
			current, err := s.Entry(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			proof := DeleteAbsenceProof{NamespaceID: op.ID, Entry: current, Parent: parent, ParentToken: parent.Token}
			switch variant {
			case "entry generation":
				proof.Entry.Generation++
			case "entry token":
				proof.Entry.Token = "replacement"
			case "parent token":
				proof.ParentToken = "different"
			case "parent generation":
				proof.Parent.Generation++
			case "wrong parent":
				proof.Parent.Remote = "somewhere-else"
			case "wrong operation":
				proof.NamespaceID = "missing"
			}
			err = s.ConfirmNamespaceDeleteAbsence(ctx, proof)
			if variant == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				actions, err := s.NamespaceActions(ctx)
				if err != nil || len(actions) != 0 {
					t.Fatal(actions, err)
				}
				if err = s.ConfirmNamespaceDeleteAbsence(ctx, proof); err != nil {
					t.Fatal("idempotent proof", err)
				}
			} else {
				if err == nil {
					t.Fatal("stale proof accepted")
				}
				journal, _ := s.NamespaceJournal(ctx, op.ID)
				if !journal.Dispatched {
					t.Fatal("unknown journal cleared")
				}
			}
		})
	}
}

func TestConfirmedDeleteAbsenceRootAndUndispatchedGuard(t *testing.T) {
	s, _, scope := openTest(t)
	e := deletionCleanEntry(t, s)
	op := deletionNamespace(t, s, e)
	if err := s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(ctx, e.ID)
	proof := DeleteAbsenceProof{NamespaceID: op.ID, Entry: current, ParentToken: scope.FolderToken}
	bad := proof
	bad.ParentToken = "wrong-root"
	if err := s.ConfirmNamespaceDeleteAbsence(ctx, bad); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	if err := s.ConfirmNamespaceDeleteAbsence(ctx, proof); err != nil {
		t.Fatal(err)
	}
	metadata, _ := s.Metadata(ctx, e.ID)
	if !metadata.Tombstone {
		t.Fatal("delete recovery resurrected local object")
	}
}

func TestLegacyDeleteParentRecoveredOnlyFromOriginalHistory(t *testing.T) {
	for _, history := range []bool{true, false} {
		t.Run(map[bool]string{true: "durable original parent", false: "missing evidence"}[history], func(t *testing.T) {
			s, _, _ := openTest(t)
			parent, err := s.ObserveEntry(ctx, Observation{Remote: "parent", Kind: model.KindFolder, Token: "old-parent"})
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.ObserveEntry(ctx, Observation{Remote: "parent/file", Kind: model.KindFile, Token: "child"})
			if err != nil {
				t.Fatal(err)
			}
			op := deletionNamespace(t, s, child)
			if err = s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
				t.Fatal(err)
			}
			// Simulate a pre-upgrade journal and a replacement at the original path.
			_ = deletionNamespace(t, s, parent)
			_, err = s.ObserveEntry(ctx, Observation{Remote: "parent", Kind: model.KindFolder, Token: "replacement"})
			if err != nil {
				t.Fatal(err)
			}
			err = s.update(ctx, func(tx *bolt.Tx) error {
				var r namespaceRecord
				if er := get(tx, "namespace", op.ID, &r); er != nil {
					return er
				}
				r.DeleteParent = nil
				if !history {
					if er := tx.DeleteBucket([]byte("history_events")); er != nil {
						return er
					}
					if _, er := tx.CreateBucket([]byte("history_events")); er != nil {
						return er
					}
				}
				return put(tx, "namespace", op.ID, r)
			})
			if err != nil {
				t.Fatal(err)
			}
			proof, err := s.NamespaceDeleteProof(ctx, op.ID)
			if !history {
				if !errors.Is(err, model.ErrUnknownResult) {
					t.Fatal("missing provenance accepted", err)
				}
				return
			}
			if err != nil || proof.Parent.ID != parent.ID || proof.ParentToken != parent.Token {
				t.Fatal("legacy lookup followed replacement", proof, err)
			}
			if err = s.ConfirmNamespaceDeleteAbsence(ctx, proof); err != nil {
				t.Fatal(err)
			}
		})
	}
}
