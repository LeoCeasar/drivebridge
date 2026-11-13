package ui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
)

type createRecoveryBackend struct {
	*fakeBackend
	calls   int
	request model.CreateRecoveryRequest
	err     error
	status  *StatusView
}

func (b *createRecoveryBackend) RecoverUpload(_ context.Context, _ model.MountID, in model.CreateRecoveryRequest) (model.CreateRecoveryResult, error) {
	b.calls++
	b.request = in
	return model.CreateRecoveryResult{JobID: in.JobID, Accepted: true}, b.err
}
func TestCreateRecoveryRouteValidatesConfirmationAndSnapshot(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	extra := &createRecoveryBackend{fakeBackend: b}
	h.backend = extra
	job := uuid.NewString()
	route := "/api/mounts/" + uuid.NewString() + "/uploads/" + job + "/recover"
	in := map[string]any{"action": "recheck", "expected_generation": uint64(4), "expected_local": strings.Repeat("a", 64)}
	post := func(body map[string]any, token string) int {
		res := request(t, c, h, "POST", route, body, token, h.URL())
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	if code := post(in, ""); code != 403 {
		t.Fatal(code)
	}
	if code := post(in, csrf); code != 200 {
		t.Fatal(code)
	}
	if extra.request.JobID != model.JobID(job) || extra.request.ExpectedGeneration != 4 {
		t.Fatal(extra.request)
	}
	in["action"] = "recreate"
	if code := post(in, csrf); code != 400 {
		t.Fatal(code)
	}
	in["confirm_recreate"] = true
	if code := post(in, csrf); code != 200 {
		t.Fatal(code)
	}
	extra.err = model.ErrGenerationChanged
	res := request(t, c, h, "POST", route, in, csrf, h.URL())
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusConflict || out["code"] != "version_changed" {
		t.Fatal(res.Status, out)
	}
	for _, field := range []string{"expected_generation", "expected_local"} {
		copy := map[string]any{}
		for k, v := range in {
			copy[k] = v
		}
		delete(copy, field)
		if code := post(copy, csrf); code != 400 {
			t.Fatal(field, code)
		}
	}
	for _, bad := range []any{"../hash", strings.Repeat("g", 64), strings.Repeat("A", 64), ""} {
		badIn := map[string]any{"action": "recheck", "expected_generation": 1, "expected_local": bad}
		if code := post(badIn, csrf); code != 400 {
			t.Fatal("bad hash", code)
		}
	}
	res = request(t, c, h, "POST", route+"?force=true", in, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal("query accepted", res.Status)
	}
	if extra.calls != 3 {
		t.Fatal(extra.calls)
	}
}

func (b *createRecoveryBackend) Status(ctx context.Context) (StatusView, error) {
	if b.status != nil {
		return *b.status, nil
	}
	return b.fakeBackend.Status(ctx)
}
func TestCreateRecoveryStatusSanitizesReasonWithoutMutatingSnapshot(t *testing.T) {
	h, b, c, _, _ := fixture(t)
	info := &model.CreateRecoveryInfo{Reason: "raw-secret-token", CurrentGeneration: 1, CurrentLocal: model.BlobID(strings.Repeat("a", 64))}
	extra := &createRecoveryBackend{fakeBackend: b, status: &StatusView{Mounts: []MountView{{State: model.MountStatus{Issues: []model.SyncIssue{{Code: "creation_needs_attention", CreateRecovery: info}}}}}}}
	h.backend = extra
	res := request(t, c, h, "GET", "/api/status", nil, "", "")
	defer res.Body.Close()
	var out StatusView
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Mounts[0].State.Issues[0].CreateRecovery.Reason != "unknown_result" || !out.CanRecoverUpload {
		t.Fatal(out)
	}
	if info.Reason != "raw-secret-token" || extra.status.Mounts[0].State.Issues[0].CreateRecovery != info {
		t.Fatal("snapshot mutated")
	}
}
