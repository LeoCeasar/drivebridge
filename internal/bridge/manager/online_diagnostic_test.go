package manager

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/lib/larkcli"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestOnlineCreationDiagnosticReachesStatus(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	en, err := s.EnsureEntry(ctx, "online.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(ctx, en.ID, nil, strings.NewReader("local"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPlan(ctx, job.ID, nil, job.Local); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareCreate(ctx, job.ID, "parent", "online")
	if err != nil {
		t.Fatal(err)
	}
	f := model.OnlineFailure{SchemaVersion: 1, ID: "id", Operation: "create", Phase: "list_parent", Cause: "auth_required", Outcome: "dispatched_unconfirmed", Action: "wait_auth", At: time.Now()}
	if _, err = s.RecordCreateObservation(ctx, op.ID, "auth_required", f, false); err != nil {
		t.Fatal(err)
	}
	status, err := storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Issues) != 1 || status.Issues[0].Online == nil || status.Issues[0].Online.Last.Cause != "auth_required" || status.Issues[0].CreateRecovery.RequestFailures != 1 {
		t.Fatalf("lost online facts: %+v", status.Issues)
	}
}

func TestErrorInfoRetainsSharedLinkFacts(t *testing.T) {
	err := online.AtPhase(errors.Join(model.ErrUnknownResult, larkcli.ErrAuthRequired), "link", "link_lookup")
	info := ErrorInfo(err)
	if info.Online == nil || info.Online.Cause != "auth_required" || info.Online.Outcome != "dispatched_unconfirmed" || info.Online.Phase != "link_lookup" {
		t.Fatalf("lost shared diagnostic: %+v", info)
	}
}
