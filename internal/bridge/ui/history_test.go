package ui

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

type historyTestBackend struct {
	*fakeBackend
	profile      model.ProfileID
	version      string
	restoreCalls int
}

func (b *historyTestBackend) HistoryList(context.Context, manager.HistoryQuery) (manager.HistoryPage, error) {
	return manager.HistoryPage{Versions: []history.Version{}}, nil
}
func (b *historyTestBackend) HistoryDetail(_ context.Context, p model.ProfileID, id string) (manager.HistoryDetail, error) {
	if p != b.profile || id != b.version {
		return manager.HistoryDetail{}, manager.ErrConfig
	}
	return manager.HistoryDetail{Version: history.Version{ID: id}}, nil
}
func (b *historyTestBackend) HistoryBody(_ context.Context, p model.ProfileID, id, role string) ([]byte, error) {
	if p != b.profile || id != b.version {
		return nil, manager.ErrConfig
	}
	return []byte("<script>not executable</script>"), nil
}
func (b *historyTestBackend) HistoryDiff(context.Context, model.ProfileID, string, string) (history.Difference, error) {
	return history.Difference{Patch: "-before\n+after"}, nil
}
func (b *historyTestBackend) HistoryRestore(context.Context, model.HistoryRestoreRequest) (model.HistoryRestoreResult, error) {
	b.restoreCalls++
	return model.HistoryRestoreResult{LocalSaved: true}, nil
}
func (b *historyTestBackend) HistoryStates(context.Context) (map[model.MountID]model.HistoryStatus, error) {
	return map[model.MountID]model.HistoryStatus{}, nil
}
func TestHistoryAPISessionGuardAndSafeText(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	extra := &historyTestBackend{fakeBackend: b, profile: model.ProfileID(uuid.NewString()), version: uuid.NewString() + ":1"}
	h.backend = extra
	req := model.HistoryRestoreRequest{ProfileID: extra.profile, MountID: model.MountID(uuid.NewString()), EntryID: model.EntryID(uuid.NewString()), ParentID: model.EntryID(uuid.NewString()), RequestID: uuid.NewString(), VersionID: extra.version}
	route := "/api/history/body?profile_id=" + string(extra.profile) + "&version_id=" + extra.version + "&role=local"
	res := request(t, c, h, "GET", route, nil, "", "")
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("safe history body: %s %s", res.Status, raw)
	}
	res = request(t, c, h, "POST", "/api/history/restore", req, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 || extra.restoreCalls != 0 {
		t.Fatal("restore bypassed CSRF")
	}
	res = request(t, c, h, "POST", "/api/history/restore", req, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || extra.restoreCalls != 1 {
		t.Fatal("authorized restore unavailable", res.Status)
	}
}
