package manager

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestBlockedUploadStatusNamesFileAndPreservesCopy(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	en, err := s.EnsureEntry(context.Background(), "folder/manual.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(context.Background(), en.ID, nil, strings.NewReader("local draft"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.PauseJob(context.Background(), job.ID, model.StatusBlocked, "unsupported_content"); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(status)
	var view struct {
		Issues []struct {
			JobID       string `json:"job_id"`
			Name        string `json:"name"`
			Code        string `json:"code"`
			LocalSaved  bool   `json:"local_saved"`
			NewDocument bool   `json:"new_document"`
		} `json:"issues"`
	}
	json.Unmarshal(raw, &view)
	if len(view.Issues) != 1 || view.Issues[0].Name != "folder/manual.md" || view.Issues[0].Code != "unsupported_content" || !view.Issues[0].LocalSaved || !view.Issues[0].NewDocument {
		t.Fatalf("blocked task missing actionable file detail: %s", raw)
	}
}

func TestPendingUploadCopyIsReadOnlyAndScoped(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	en, err := s.EnsureEntry(ctx, "manual.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(ctx, en.ID, nil, strings.NewReader("local draft <img/>"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	copy, err := m.UploadLocalCopy(ctx, id, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copy.Name != "manual.md" || string(copy.Content) != "local draft <img/>" {
		t.Fatal("copy changed captured bytes")
	}
	after, err := s.Job(ctx, job.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("download changed upload state", err)
	}
	if _, err := m.UploadLocalCopy(ctx, id, model.JobID(uuid.NewString())); err == nil {
		t.Fatal("unknown job accepted")
	}
}
