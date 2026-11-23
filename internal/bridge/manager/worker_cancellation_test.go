package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
)

func TestWorkerControlsReportActualCancellationBoundary(t *testing.T) {
	for _, mode := range []string{"caller_gone", "parent_deadline", "runtime_shutdown"} {
		t.Run(mode, func(t *testing.T) {
			w := &worker{ready: true}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var deadlineCancel context.CancelFunc
			if mode == "parent_deadline" {
				parent, deadlineCancel = context.WithDeadline(parent, time.Now().Add(-time.Second))
				defer deadlineCancel()
			}
			ctx, release, err := w.beginControl(parent)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			if mode == "runtime_shutdown" {
				go func() { w.closeControls(); close(done) }()
			} else {
				cancel()
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("control not canceled")
			}
			f := online.Capture(ctx, ctx.Err(), "sync", "readback", "", time.Now())
			release()
			if mode == "runtime_shutdown" {
				<-done
			}
			if f.CancelOrigin != mode {
				t.Fatalf("boundary=%s failure=%+v", mode, f)
			}
			if mode == "parent_deadline" && f.Cause != "request_timeout" {
				t.Fatalf("deadline cause=%+v", f)
			}
		})
	}
}

func TestWorkerEngineReportsUserStopVersusRuntimeShutdown(t *testing.T) {
	for _, mode := range []string{"user_stop", "runtime_shutdown", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			m, id, _, cancel := fixture(t, nil)
			original := m.factory
			failures := make(chan model.OnlineFailure, 1)
			m.factory = func(ctx context.Context, spec mountSpec) (*workerServices, error) {
				s, err := original(ctx, spec)
				if err != nil {
					return nil, err
				}
				s.run = func(ctx context.Context) error {
					<-ctx.Done()
					failures <- online.Capture(ctx, ctx.Err(), "sync", "readback", "", time.Now())
					return ctx.Err()
				}
				return s, nil
			}
			if _, err := m.Start(context.Background(), id, true); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "user_stop":
				err = m.Stop(context.Background(), id, time.Second)
			case "maintenance":
				err = m.stopAdmitted(context.Background(), id, time.Second)
			default:
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case f := <-failures:
				want := mode
				if want == "maintenance" {
					want = "runtime_shutdown"
				}
				if f.CancelOrigin != want {
					t.Fatalf("boundary=%s failure=%+v", mode, f)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("engine not canceled")
			}
		})
	}
}

func TestWorkerStartResumesOnlyExplicitUserStopAndKeepsUnknownWrite(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	original := m.factory
	jobs := map[string]model.JobID{}
	prepared := make(chan struct{})
	m.factory = func(ctx context.Context, spec mountSpec) (*workerServices, error) {
		s, err := original(ctx, spec)
		if err != nil {
			return nil, err
		}
		for _, kind := range []string{"user_stop", "unknown_write", "unspecified_cancel"} {
			en, er := s.store.EnsureEntry(ctx, kind, model.KindFile)
			if er != nil {
				return nil, er
			}
			_, job, er := s.store.Stage(ctx, en.ID, nil, strings.NewReader("saved body"))
			if er != nil {
				return nil, er
			}
			if _, _, er = s.store.Prepare(ctx, job.ID); er != nil {
				return nil, er
			}
			origin := "user_stop"
			outcome := "not_dispatched"
			if kind == "unknown_write" {
				er = s.store.MarkUnknown(ctx, job.ID)
				outcome = "dispatched_unconfirmed"
			} else {
				er = s.store.PauseJob(ctx, job.ID, model.StatusBlocked, "canceled")
			}
			if er != nil {
				return nil, er
			}
			if kind == "unspecified_cancel" {
				origin = kind
			}
			cancelCtx, cancel := context.WithCancel(online.WithCancelOrigin(ctx, origin))
			cancel()
			if er = s.store.RecordJobOnlineFailure(ctx, job.ID, online.Capture(cancelCtx, cancelCtx.Err(), "sync", "readback", outcome, time.Now())); er != nil {
				return nil, er
			}
			jobs[kind] = job.ID
		}
		close(prepared)
		return s, nil
	}
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	<-prepared
	w := m.localWorker(id)
	for kind, jobID := range jobs {
		j, err := w.services.store.Journal(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		wantState := model.StatusQueued
		if kind == "unknown_write" {
			wantState = model.StatusAwaitingVerification
		}
		if kind == "unspecified_cancel" {
			wantState = model.StatusBlocked
		}
		if j.Job.State != wantState {
			t.Fatalf("kind=%s state=%s want=%s", kind, j.Job.State, wantState)
		}
		if kind != "unspecified_cancel" && j.Online.PausedReason != "" {
			t.Fatalf("explicit start did not clear user stop: %+v", j.Online)
		}
		if kind == "unspecified_cancel" && j.Online.PausedReason != "cancel_unspecified_cancel" {
			t.Fatalf("start released unknown cancellation: %+v", j.Online)
		}
	}
}

func TestStartingWorkerDoesNotPromotePersistedJobErrorToStartupFailure(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.factory(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer s.store.Close()
	en, err := s.store.EnsureEntry(context.Background(), "saved", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.store.Stage(context.Background(), en.ID, nil, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.store.Prepare(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.store.PauseJob(context.Background(), job.ID, model.StatusBlocked, "canceled"); err != nil {
		t.Fatal(err)
	}
	w := &worker{services: s, spec: spec}
	state, err := w.status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.LastError != "" {
		t.Fatalf("persisted job error was exposed as startup failure: %s", state.LastError)
	}
	w.ready = true
	state, err = w.status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.LastError != "canceled" {
		t.Fatalf("ready worker hid per-job failure: %+v", state)
	}
}
