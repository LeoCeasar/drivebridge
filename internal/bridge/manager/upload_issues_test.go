package manager

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if err = s.PauseJobWithDiagnostic(context.Background(), job.ID, model.StatusBlocked, "unsupported_content", &model.SyncDiagnostic{Kind: "html_block", Message: "HTML block cannot be written back", Line: 2, EndLine: 3, Text: "<div>blocked</div>", Side: "local"}); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(status)
	var view struct {
		Issues []struct {
			Diagnostic  *model.SyncDiagnostic `json:"diagnostic"`
			JobID       string                `json:"job_id"`
			Name        string                `json:"name"`
			Code        string                `json:"code"`
			LocalSaved  bool                  `json:"local_saved"`
			NewDocument bool                  `json:"new_document"`
		} `json:"issues"`
	}
	json.Unmarshal(raw, &view)
	if len(view.Issues) != 1 || view.Issues[0].Name != "folder/manual.md" || view.Issues[0].Code != "unsupported_content" || !view.Issues[0].LocalSaved || !view.Issues[0].NewDocument || view.Issues[0].Diagnostic == nil || view.Issues[0].Diagnostic.Line != 2 || view.Issues[0].Diagnostic.Text != "<div>blocked</div>" {
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

func TestAwaitingCreationIsVisibleAndDoesNotMaskActiveUpload(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	en, err := s.EnsureEntry(ctx, "uncertain.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(ctx, en.ID, nil, strings.NewReader("draft"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPlan(ctx, job.ID, nil, job.Local); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareCreate(ctx, job.ID, "parent", "uncertain")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkCreateUnknown(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	status, err := storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	if status.CloudState != "awaiting_verification" || status.Inflight != 0 || len(status.Issues) != 1 || status.Issues[0].CreateRecovery == nil || status.Issues[0].Code != "unknown_result" {
		t.Fatalf("uncertain status = %+v", status)
	}
	if status.Issues[0].Diagnostic == nil || !strings.Contains(status.Issues[0].Diagnostic.Message, "旧记录未保留首次错误原因") {
		t.Fatal("missing legacy explanation")
	}
	if status.Issues[0].CreateRecovery.CurrentGeneration != job.Generation || status.Issues[0].CreateRecovery.CurrentLocal != job.Local {
		t.Fatal("missing current snapshot")
	}
	other, err := s.EnsureEntry(ctx, "active.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, active, err := s.Stage(ctx, other.ID, nil, strings.NewReader("active"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	status, err = storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	if status.CloudState != "uploading" || status.Inflight != 1 {
		t.Fatalf("actual activity masked = %+v", status)
	}
}

func TestCreationAttentionStatusUsesCurrentLocalAndKeepsOriginalCopy(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	en, err := s.EnsureEntry(ctx, "pending.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(ctx, en.ID, nil, strings.NewReader("original body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPlan(ctx, job.ID, nil, job.Local); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareCreate(ctx, job.ID, "parent", "pending")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = s.MarkCreateUncertain(ctx, op.ID, "", "", "request_timeout", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = s.RecordCreateCheck(ctx, op.ID, "not_found", "network_or_protocol", now); err != nil {
			t.Fatal(err)
		}
	}
	status, err := storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	if status.CloudState != "blocked" || status.Inflight != 0 || len(status.Issues) != 1 || !status.Issues[0].CreateRecovery.CanRecreate || status.Issues[0].CreateRecovery.Attempts != 3 {
		t.Fatalf("attention = %+v", status)
	}
	latest, _, err := s.Stage(ctx, en.ID, nil, strings.NewReader("latest body"))
	if err != nil {
		t.Fatal(err)
	}
	status, err = storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	info := status.Issues[0].CreateRecovery
	if info.CurrentGeneration != latest.Generation || info.CurrentLocal != latest.Local || info.CurrentLocal == job.Local {
		t.Fatal("recovery must use latest snapshot", info, latest)
	}
	copy, err := pendingUploadCopy(ctx, s, job.ID)
	if err != nil || string(copy.Content) != "original body" {
		t.Fatal("original copy lost", copy, err)
	}
	if !strings.Contains(status.Issues[0].Diagnostic.Message, "首次创建请求超时") || !strings.Contains(status.Issues[0].Diagnostic.Message, "尚未找到") {
		t.Fatal(status.Issues[0].Diagnostic)
	}
	if err = s.MarkCreateUncertain(ctx, op.ID, "private-token-never-expose", "", "request_timeout", now); err != nil {
		t.Fatal(err)
	}
	status, err = storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(status)
	if status.Issues[0].CreateRecovery.CanRecreate || strings.Contains(string(raw), "private-token-never-expose") {
		t.Fatal("candidate was exposed or allowed recreation")
	}
}

func TestPreparedCreationAttentionPermitsExplicitRecreation(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	en, err := s.EnsureEntry(ctx, "prepared.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	_, job, err := s.Stage(ctx, en.ID, nil, strings.NewReader("preserved body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPlan(ctx, job.ID, nil, job.Local); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareCreate(ctx, job.ID, "parent", "prepared")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if op, err = s.RecordCreateCheck(ctx, op.ID, "not_found", "unknown_result", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if op.State != "prepared" || !op.NeedsAttention {
		t.Fatalf("unexpected journal %+v", op)
	}
	status, err := storeStatus(ctx, id, s)
	if err != nil {
		t.Fatal(err)
	}
	if status.CloudState != "blocked" || status.Inflight != 0 || len(status.Issues) != 1 || status.Issues[0].CreateRecovery == nil || !status.Issues[0].CreateRecovery.CanRecreate {
		t.Fatalf("prepared creation cannot recover: %+v", status)
	}
}
