package store

import (
	"errors"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func TestNamespaceCancellationWriteGateSurvivesReopenAndExplicitRetryPreservesFacts(t *testing.T) {
	s, root, scope := openTest(t)
	entry := deletionCleanEntry(t, s)
	op := deletionNamespace(t, s, entry)
	failure := model.OnlineFailure{SchemaVersion: 1, ID: "cancel-diagnostic", Cause: "canceled", CancelOrigin: "user_stop", Operation: "delete", Phase: "delete_dispatch", Outcome: "not_dispatched", At: time.Now().UTC()}
	if err := s.RecordNamespaceOnlineFailure(ctx, op.ID, failure); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err = reopened.BeginNamespaceDispatch(ctx, op.ID, "delete"); !errors.Is(err, ErrNamespaceWritePaused) {
		t.Fatalf("restart released cancellation write gate: %v", err)
	}
	before, err := reopened.NamespaceJournal(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.ClearNamespaceOnlinePauses(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := reopened.NamespaceJournal(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Dispatched || after.ActionPending != before.ActionPending || after.Op.ID != before.Op.ID || after.Online.First.ID != failure.ID || after.Online.Last.ID != failure.ID || after.Online.PausedReason != "" {
		t.Fatalf("continuation discarded facts or changed operation: before=%+v after=%+v", before, after)
	}
	if err = reopened.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
		t.Fatal("explicit continuation cannot enter normal dispatch validation", err)
	}
}

func TestNamespaceRetryRetainsNonCancellationPauseAndServiceBounds(t *testing.T) {
	s, _, _ := openTest(t)
	entry := deletionCleanEntry(t, s)
	op := deletionNamespace(t, s, entry)
	if err := s.RecordNamespaceOnlineFailure(ctx, op.ID, model.OnlineFailure{Cause: "identity_changed", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(time.Hour).UTC()
	if err := s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", op.ID, &r); err != nil {
			return err
		}
		r.Op.Online.PausedReason = "identity_changed"
		r.Op.Online.NotBefore = due
		return put(tx, "namespace", op.ID, r)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNamespaceOnlineFailure(ctx, op.ID, model.OnlineFailure{Cause: "canceled", CancelOrigin: "unspecified_cancel", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearNamespaceOnlinePauses(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.NamespaceJournal(ctx, op.ID)
	if err != nil || after.Online.PausedReason != "identity_changed" || !after.Online.NotBefore.Equal(due) {
		t.Fatalf("retry released non-cancellation restriction: %+v %v", after.Online, err)
	}
}
