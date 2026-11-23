package syncer

import (
	"context"
	"errors"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestNamespaceOnlineListFailureKeepsPhaseAndNoDispatch(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	source := &namespaceRecoveryCloud{fixtureCloud: c, listErr: larkcli.ErrAuthRequired}
	e.source = phasedSource{source}
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	if err := e.syncNamespace(bg); !errors.Is(err, larkcli.ErrAuthRequired) {
		t.Fatal(err)
	}
	work, err := s.NamespaceJournal(bg, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Online == nil || work.Online.Last == nil {
		t.Fatal("namespace failure diagnostic missing")
	}
	last := work.Online.Last
	if last.Cause != "auth_required" || last.Phase != "list_parent" || last.Operation != "delete" || last.Outcome != "not_dispatched" {
		t.Fatalf("diagnostic=%+v", last)
	}
	if !work.ActionPending || work.Dispatched || c.deletes != 0 {
		t.Fatalf("diagnostic altered business work: %+v deletes=%d", work, c.deletes)
	}
}

func TestNamespaceOnlineUnknownDeleteThenAuthRetainsUnknownAndResolves(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	source := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = phasedSource{source}
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	if err := e.syncNamespace(bg); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	first, _ := s.NamespaceJournal(bg, op.ID)
	if first.Online == nil || first.Online.First == nil {
		t.Fatal("first unknown deletion diagnostic missing")
	}
	firstID := first.Online.First.ID
	source.listErr = larkcli.ErrAuthRequired
	if err := e.syncNamespace(bg); !errors.Is(err, larkcli.ErrAuthRequired) {
		t.Fatal(err)
	}
	work, _ := s.NamespaceJournal(bg, op.ID)
	if work.Online == nil || work.Online.Last == nil || work.Online.Last.Cause != "auth_required" || work.Online.Last.Outcome != "dispatched_unconfirmed" || work.Online.Last.Phase != "list_parent" {
		t.Fatalf("latest auth hid unknown delete: %+v", work.Online)
	}
	if work.Online.First.ID != firstID || !work.ActionPending || !work.Dispatched || c.deletes != 1 {
		t.Fatalf("history or dispatch changed: %+v deletes=%d", work, c.deletes)
	}
	source.listErr = nil
	c.items = nil // Complete original-parent observation proves removal; no second delete.
	if err := e.syncNamespace(bg); err != nil {
		t.Fatal(err)
	}
	completed, _ := s.NamespaceJournal(bg, op.ID)
	if completed.Online == nil || !completed.Online.Resolved || completed.Online.First.ID != firstID || completed.ActionPending || completed.Dispatched || c.deletes != 1 {
		t.Fatalf("completion must resolve diagnostic and preserve history: %+v deletes=%d", completed, c.deletes)
	}
}

func TestNamespaceOnlineIdentityFailureBeforeDispatch(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "rename", Src: entry.Remote, Dst: "renamed.md"})
	e.deps.CheckIdentity = func(_ context.Context) error { return model.ErrIdentityChanged }
	if err := e.syncNamespace(bg); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	work, _ := s.NamespaceJournal(bg, op.ID)
	if work.Online == nil || work.Online.Last == nil || work.Online.Last.Phase != "identity_check" || work.Online.Last.Cause != "identity_changed" || work.Dispatched || c.writes != 0 {
		t.Fatalf("identity failure missing or dispatched: %+v", work)
	}
}

func TestNamespaceOnlineRenameResolvesOnlyAfterAcknowledged(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	source := &namespaceRecoveryCloud{fixtureCloud: c, listErr: larkcli.ErrAuthRequired}
	e.source = phasedSource{source}
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "rename", Src: entry.Remote, Dst: "renamed.md"})
	if err := e.syncNamespace(bg); !errors.Is(err, larkcli.ErrAuthRequired) {
		t.Fatal(err)
	}
	source.listErr = nil
	if err := e.syncNamespace(bg); err != nil {
		t.Fatal(err)
	}
	work, _ := s.NamespaceJournal(bg, op.ID)
	if work.Online == nil || !work.Online.Resolved || work.Online.First == nil || work.ActionPending {
		t.Fatalf("acknowledged rename diagnostic unresolved: %+v", work)
	}
}

type namespaceCancellationCloud struct {
	*namespaceRecoveryCloud
	renameCalls    int
	folderCanceled bool
	cancel         context.CancelFunc
}

func (c *namespaceCancellationCloud) Rename(ctx context.Context, token string, kind model.Kind, name string) error {
	c.renameCalls++
	if c.renameCalls == 1 && !c.folderCanceled {
		if c.cancel != nil {
			c.cancel()
		}
		return errors.Join(cloud.ErrNotDispatched, context.Canceled)
	}
	return c.fixtureCloud.Rename(ctx, token, kind, name)
}
func (c *namespaceCancellationCloud) CreateFolder(ctx context.Context, parent, name string) (string, error) {
	token, err := c.namespaceRecoveryCloud.CreateFolder(ctx, parent, name)
	if err != nil {
		return token, err
	}
	c.folderCanceled = true
	return "", errors.Join(model.ErrUnknownResult, context.Canceled)
}
func clearNamespaceCancellationForTest(t *testing.T, s *store.Store) {
	t.Helper()
	clearer, ok := any(s).(interface{ ClearNamespaceOnlinePauses(context.Context) error })
	if !ok {
		t.Fatal("explicit namespace cancellation continuation missing")
	}
	if err := clearer.ClearNamespaceOnlinePauses(bg); err != nil {
		t.Fatal(err)
	}
}
func TestNamespaceOnlineCanceledUndispatchedWriteWaitsForExplicitRetry(t *testing.T) {
	for _, origin := range []string{"user_stop", "unspecified_cancel"} {
		t.Run(origin, func(t *testing.T) {
			s, e, c, entry := setup(t, model.KindFile)
			source := &namespaceCancellationCloud{namespaceRecoveryCloud: &namespaceRecoveryCloud{fixtureCloud: c}}
			e.source = phasedSource{source}
			op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "rename", Src: entry.Remote, Dst: "after.md"})
			cancelCtx, cancel := context.WithCancel(online.WithCancelOrigin(bg, origin))
			defer cancel()
			source.cancel = cancel
			if err := e.syncNamespace(cancelCtx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			initial, _ := s.NamespaceJournal(bg, op.ID)
			if initial.Online == nil || initial.Online.PausedReason != "cancel_"+origin || initial.Dispatched {
				t.Fatalf("cancellation not durably restricted: %+v online=%+v last=%+v", initial, initial.Online, initial.Online.Last)
			}
			if err := e.syncNamespace(bg); err != nil {
				t.Fatal(err)
			}
			paused, _ := s.NamespaceJournal(bg, op.ID)
			if source.renameCalls != 1 || !paused.ActionPending {
				t.Fatalf("canceled namespace mutation repeated: calls=%d journal=%+v", source.renameCalls, paused)
			}
			clearNamespaceCancellationForTest(t, s)
			if err := e.syncNamespace(bg); err != nil {
				t.Fatal(err)
			}
			completed, _ := s.NamespaceJournal(bg, op.ID)
			if source.renameCalls != 2 || completed.ActionPending || !completed.Online.Resolved {
				t.Fatalf("explicit continuation did not use normal validation: calls=%d journal=%+v", source.renameCalls, completed)
			}
		})
	}
}
func TestNamespaceOnlineCanceledUnknownFolderObservesWithoutRemainingRename(t *testing.T) {
	s, e, c, _ := setup(t, model.KindFile)
	source := &namespaceCancellationCloud{namespaceRecoveryCloud: &namespaceRecoveryCloud{fixtureCloud: c}}
	e.source = phasedSource{source}
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "new-folder"})
	if err := e.syncNamespace(online.WithCancelOrigin(bg, "unspecified_cancel")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	first, _ := s.NamespaceJournal(bg, op.ID)
	if !first.Dispatched {
		t.Fatal("test must begin with unknown folder create")
	}
	_ = e.syncNamespace(bg)
	observed, _ := s.NamespaceJournal(bg, op.ID)
	if source.renameCalls != 0 || source.folders != 1 || !observed.ActionPending || !observed.Completed["create_folder"] || observed.Online.PausedReason != "cancel_unspecified_cancel" {
		t.Fatalf("observation allowed a remaining mutation: calls=%d journal=%+v", source.renameCalls, observed)
	}
	clearNamespaceCancellationForTest(t, s)
	if err := e.syncNamespace(bg); err != nil {
		t.Fatal(err)
	}
	completed, _ := s.NamespaceJournal(bg, op.ID)
	if source.renameCalls != 1 || source.folders != 1 || completed.ActionPending {
		t.Fatalf("continuation replayed creation or failed rename: %+v", completed)
	}
}
