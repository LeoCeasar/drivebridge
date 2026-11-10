package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestStaleConflictKeepsSourcesAndDraftButRequiresRefresh(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	b.input.Stale = true
	b.input.RefreshWarning = "raw CLI secret should never reach browser"
	id := string(b.input.Conflict.ID)
	res := request(t, c, h, "GET", "/api/conflicts/"+id, nil, "", "")
	var detail ConflictView
	if err := json.NewDecoder(res.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || !detail.Stale || !detail.Text || detail.Local != string(b.input.Local) || detail.RefreshWarning != "operation_failed" {
		t.Fatal(detail, res.Status)
	}
	draft := []byte("offline chosen 中文😀\n")
	res = request(t, c, h, "PUT", "/api/conflicts/"+id+"/draft", map[string][]byte{"content": draft}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	r := model.ResolveRequest{ConflictID: b.input.Conflict.ID, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1", Content: draft}
	res = request(t, c, h, "POST", "/api/conflicts/"+id+"/resolve", r, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 503 || b.resolves != 0 {
		t.Fatal("cached sources were submitted without refresh", res.Status, b.resolves)
	}
	saved, err := os.ReadFile(b.draftFile)
	if err != nil || !bytes.Equal(saved, draft) {
		t.Fatal(string(saved), err)
	}
	b.input.Stale = false
	b.resolveErr = model.ErrVersionChanged
	r.Content = []byte("draft after concurrent change\n")
	res = request(t, c, h, "POST", "/api/conflicts/"+id+"/resolve", r, csrf, h.URL())
	res.Body.Close()
	saved, err = os.ReadFile(b.draftFile)
	if res.StatusCode != 409 || err != nil || !bytes.Equal(saved, r.Content) {
		t.Fatal(res.Status, string(saved), err)
	}
}

type recoveryBackend struct {
	*fakeBackend
	retried  model.MountID
	exported model.ConflictID
	copies   ConflictCopies
}

func (b *recoveryBackend) Retry(_ context.Context, id model.MountID) error {
	b.retried = id
	return nil
}
func (b *recoveryBackend) ExportConflict(_ context.Context, id model.ConflictID) (ConflictCopies, error) {
	b.exported = id
	return b.copies, nil
}

func TestRecoveryActionsRequireCapabilityAndExplicitAuthenticatedRequest(t *testing.T) {
	h, b, c, csrf, opts := fixture(t)
	mount := model.MountID(uuid.NewString())
	retryRoute := "/api/mounts/" + string(mount) + "/retry"
	copyRoute := "/api/conflicts/" + string(b.input.Conflict.ID) + "/copies"
	for _, route := range []string{retryRoute, copyRoute} {
		res := request(t, c, h, "POST", route, struct{}{}, csrf, h.URL())
		res.Body.Close()
		if res.StatusCode != 503 {
			t.Fatal(res.Status)
		}
	}
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
	backend := &recoveryBackend{fakeBackend: b, copies: ConflictCopies{Directory: filepath.Dir(b.draftFile), Local: b.draftFile, Manifest: filepath.Join(filepath.Dir(b.draftFile), "manifest.json")}}
	opts.Backend = backend
	h, err := Start(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(ctx) })
	c, csrf = bootstrap(t, h)
	res := request(t, c, h, "GET", "/api/status", nil, "", "")
	var status StatusView
	if err = json.NewDecoder(res.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !status.CanRetry {
		t.Fatal("retry capability hidden")
	}
	res = request(t, c, h, "GET", "/api/conflicts/"+string(b.input.Conflict.ID), nil, "", "")
	var detail ConflictView
	if err = json.NewDecoder(res.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !detail.CanExportCopies || backend.exported != "" {
		t.Fatal("detail must advertise without exporting", detail)
	}
	res = request(t, c, h, "GET", copyRoute, nil, "", "")
	res.Body.Close()
	if res.StatusCode != 405 || backend.exported != "" {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", copyRoute, map[string]string{"path": "../../arbitrary"}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 400 || backend.exported != "" {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", copyRoute, struct{}{}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 || backend.exported != "" {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", retryRoute, struct{}{}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || backend.retried != mount {
		t.Fatal(res.Status, backend.retried)
	}
	res = request(t, c, h, "POST", copyRoute, struct{}{}, csrf, h.URL())
	var copies ConflictCopies
	if err = json.NewDecoder(res.Body).Decode(&copies); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || backend.exported != b.input.Conflict.ID || copies != backend.copies {
		t.Fatal(res.Status, copies)
	}
}
