package manager

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestRecoverUploadRequiresActiveWorkerAndValidatedInput(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	req := model.CreateRecoveryRequest{JobID: model.JobID(uuid.NewString()), Action: "recheck", ExpectedGeneration: 1, ExpectedLocal: model.BlobID(strings.Repeat("a", 64))}
	if _, err := m.RecoverUpload(context.Background(), id, req); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped recovery = %v", err)
	}
	req.Action = "recreate"
	if _, err := m.RecoverUpload(context.Background(), id, req); !errors.Is(err, ErrConfig) {
		t.Fatalf("unconfirmed recovery = %v", err)
	}
}
func TestRecoveryIPCUsesControlAndRejectsInvalidInput(t *testing.T) {
	calls := 0
	w := &worker{ready: true, controls: map[uint64]context.CancelFunc{}, services: &workerServices{recoverCreation: func(_ context.Context, in model.CreateRecoveryRequest) (model.CreateRecoveryResult, error) {
		calls++
		return model.CreateRecoveryResult{JobID: in.JobID, Accepted: true}, nil
	}}}
	body := `{"job_id":"` + uuid.NewString() + `","action":"recheck","expected_generation":1,"expected_local":"` + strings.Repeat("a", 64) + `"}`
	if _, err := w.dispatch(httptest.NewRequest("POST", "/upload-recover", strings.NewReader(body))); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if _, err := w.dispatch(httptest.NewRequest("POST", "/upload-recover", strings.NewReader(strings.Replace(body, "recheck", "recreate", 1)))); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("invalid input reached engine")
	}
}

func TestRecoveryControlIsCanceledBeforeWorkerStops(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan error, 1)
	w := &worker{ready: true, services: &workerServices{recoverCreation: func(ctx context.Context, _ model.CreateRecoveryRequest) (model.CreateRecoveryResult, error) {
		close(entered)
		<-ctx.Done()
		return model.CreateRecoveryResult{}, ctx.Err()
	}}}
	body := `{"job_id":"` + uuid.NewString() + `","action":"recheck","expected_generation":1,"expected_local":"` + strings.Repeat("a", 64) + `"}`
	go func() {
		_, err := w.dispatch(httptest.NewRequest("POST", "/upload-recover", strings.NewReader(body)))
		finished <- err
	}()
	<-entered
	w.closeControls()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := w.dispatch(httptest.NewRequest("POST", "/upload-recover", strings.NewReader(body))); !errors.Is(err, ErrReadyTimeout) {
		t.Fatal(err)
	}
}

func TestRecoverUploadCallsActiveWorker(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	req := model.CreateRecoveryRequest{JobID: model.JobID(uuid.NewString()), Action: "recreate", ExpectedGeneration: 8, ExpectedLocal: model.BlobID(strings.Repeat("a", 64)), ConfirmRecreate: true}
	w := m.localWorker(id)
	w.mu.Lock()
	w.services.recoverCreation = func(_ context.Context, got model.CreateRecoveryRequest) (model.CreateRecoveryResult, error) {
		if got != req {
			return model.CreateRecoveryResult{}, ErrConfig
		}
		return model.CreateRecoveryResult{JobID: got.JobID, Generation: 9, Recreated: true, Accepted: true}, nil
	}
	w.mu.Unlock()
	out, err := m.RecoverUpload(ctx, id, req)
	if err != nil || !out.Accepted || !out.Recreated || out.Generation != 9 {
		t.Fatal(out, err)
	}
}
