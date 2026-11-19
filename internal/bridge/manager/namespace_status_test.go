package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestPendingParentIsQueuedWithoutNetworkIssue(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.EnsureEntry(ctx, "child.txt", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(ctx, e.ID, nil, strings.NewReader("saved"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseJobWithDiagnostic(ctx, job.ID, model.StatusPaused, "parent_pending", &model.SyncDiagnostic{Kind: "parent_pending", Message: "等待上级文件夹"}); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(ctx, id)
	if err != nil || status.WaitingForParent != 1 || status.LastError != "" || len(status.Issues) != 0 || status.CloudState != "queued" || status.Queued != 1 {
		t.Fatalf("waiting is not an error: %+v %v", status, err)
	}
}

func TestNamespaceDeleteUncertaintyIsVisibleAndNotActiveUpload(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(ctx, store.Observation{Remote: "old-test.txt", Kind: model.KindFile, Token: "old"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: e.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginNamespaceDispatch(ctx, op.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(ctx, id)
	if err != nil || status.Inflight != 0 || status.CloudState != "awaiting_verification" || len(status.Issues) != 1 || status.Issues[0].Code != "delete_result_unconfirmed" || status.Issues[0].NamespaceID != op.ID {
		t.Fatalf("uncertain delete hidden or shown as active upload: %+v %v", status, err)
	}
}
