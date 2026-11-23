package syncer

import (
	"context"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

type failingCreationList struct {
	Source
	err error
}

func (s failingCreationList) List(context.Context, string) ([]larkcli.DriveItem, error) {
	return nil, s.err
}

func TestCreationManualRecheckDoesNotDispatchRename(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	if _, err := e.RecoverCreation(bg, recoveryRequest(en, job, "recheck")); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, item := range source.items {
		if item.Token == "created-file" {
			found = item.Name
		}
	}
	if found != op.TemporaryName {
		t.Fatalf("read-only recheck dispatched rename: got %q want %q", found, op.TemporaryName)
	}
	current, _ := s.Entry(bg, en.ID)
	if current.Token != "" || source.writes != 1 {
		t.Fatal("observer bound or rewrote resource", current.Token, source.writes)
	}
	observed, _ := s.CreateForJob(bg, job.ID)
	if !observed.ObservationReady {
		t.Fatal("verified observation not retained")
	}
	// Only a later normal business pass can continue the original rename.
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Entry(bg, en.ID)
	if current.Status != model.StatusSynced || source.writes != 1 {
		t.Fatal(current.Status, source.writes)
	}
}

func TestCreationAuthChecksDoNotExhaustValidBudget(t *testing.T) {
	s, e, source, en, job, _ := unknownCreateFixture(t)
	e.source = failingCreationList{Source: source, err: larkcli.ErrAuthRequired}
	now := time.Now()
	e.opt.Now = func() time.Time { return now }
	for range 5 {
		_ = e.SyncEntry(bg, en.ID)
		now = now.Add(5 * time.Minute)
	}
	op, _ := s.CreateForJob(bg, job.ID)
	if op.NeedsAttention || op.EffectiveChecks != 0 || op.RequestFailures != 5 {
		t.Fatalf("auth consumed valid checks: %+v", op)
	}
	j, _ := s.Journal(bg, job.ID)
	if j.Online == nil || j.Online.Last.Cause != "auth_required" || j.Online.Last.Phase != "list_parent" || j.Online.Last.Outcome != "dispatched_unconfirmed" {
		t.Fatalf("lost precise diagnostic: %+v", j.Online)
	}
}

func TestDueCreationObservationDoesNotAdmitCloudWrites(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	now := time.Now()
	e.opt.Now = func() time.Time { return now }
	f := createObservationFailure(bg, larkcli.ErrAuthRequired, "auth_required", now)
	if _, err := s.RecordCreateObservation(bg, op.ID, "auth_required", f, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	d := newBackgroundDispatcher(ctx, e)
	defer d.close()
	now = now.Add(10 * time.Second)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := d.tick(); err != nil {
			t.Fatal(err)
		}
		current, _ := s.CreateForJob(bg, job.ID)
		if current.ObservationReady {
			for _, item := range source.items {
				if item.Token == "created-file" && item.Name != op.TemporaryName {
					t.Fatal("due observation renamed cloud file")
				}
			}
			entry, _ := s.Entry(bg, en.ID)
			if entry.Token != "" {
				t.Fatal("due observation bound resource")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("due observer waited for five-minute write admission")
}

func TestRecheckAdmissionRemainsReadOnlyAfterConcurrentObservation(t *testing.T) {
	s, e, source, en, job, op := unknownCreateFixture(t)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	d := newBackgroundDispatcher(ctx, e)
	defer d.close()
	e.dispatchMu.Lock()
	e.dispatcher = d
	e.dispatchMu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := e.RecoverCreation(ctx, recoveryRequest(en, job, "recheck")); done <- err }()
	var request dispatchRequest
	select {
	case request = <-d.requests:
	case <-time.After(time.Second):
		t.Fatal("no recheck admission")
	}
	// A due observer finishes between committing recheck and accepting its request.
	if err := s.RecordCreationObserved(bg, op.ID, "created-file", "r1"); err != nil {
		t.Fatal(err)
	}
	d.accept(request)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.opt.ForegroundGrace = -1
	d.pump()
	select {
	case result := <-d.results:
		d.completed(result)
	case <-time.After(time.Second):
		t.Fatal("no observation completion")
	}
	for _, item := range source.items {
		if item.Token == "created-file" && item.Name != op.TemporaryName {
			t.Fatal("recheck raced into rename")
		}
	}
	if len(d.entries) != 0 {
		t.Fatal("observation retained writable admission")
	}
}

func TestLocalObservationReadFailurePersistsRepairAndDue(t *testing.T) {
	s, e, _, _, job, op := unknownCreateFixture(t)
	op.Kind = model.KindDocx
	op.Input = model.BlobID("0000000000000000000000000000000000000000000000000000000000000000")
	if err := e.observePendingCreation(bg, job, op); err == nil {
		t.Fatal("missing local read failure")
	}
	current, err := s.CreateForJob(bg, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.Journal(bg, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.NextCheckAt.After(e.now()) || j.Online == nil || j.Online.Last.Phase != "local_commit" || j.Online.Last.Action != "repair_local" {
		t.Fatalf("read failure can spin without repair diagnostic: op=%+v online=%+v", current, j.Online)
	}
}
