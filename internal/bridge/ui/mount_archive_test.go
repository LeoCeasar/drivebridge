package ui

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"net/http"
	"testing"
)

type archiveActionFixture struct {
	*fakeBackend
	actions []string
	busy    bool
	failure error
}

func (b *archiveActionFixture) Mount(_ context.Context, _ model.MountID, action string) error {
	b.actions = append(b.actions, action)
	if b.failure != nil {
		return b.failure
	}
	if b.busy {
		return manager.ErrPending
	}
	return nil
}
func TestMountArchiveRestoreStayAuthenticatedAndLocal(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	source := &archiveActionFixture{fakeBackend: b}
	h.backend = source
	id := uuid.NewString()
	for _, action := range []string{"archive", "restore", "delete", "restore_start"} {
		res := request(t, c, h, "POST", "/api/mounts/"+id+"/"+action, map[string]any{}, csrf, h.URL())
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.Status)
		}
	}
	if len(source.actions) != 4 || source.actions[0] != "archive" || source.actions[1] != "restore" || source.actions[2] != "delete" || source.actions[3] != "restore_start" {
		t.Fatal(source.actions)
	}
	res := request(t, c, h, "POST", "/api/mounts/"+id+"/delete", map[string]any{}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 || len(source.actions) != 4 {
		t.Fatal("unauthorized archive", res.Status, source.actions)
	}
	source.busy = true
	res = request(t, c, h, "POST", "/api/mounts/"+id+"/archive", map[string]any{}, csrf, h.URL())
	defer res.Body.Close()
	var got map[string]any
	json.NewDecoder(res.Body).Decode(&got)
	if res.StatusCode != http.StatusConflict || got["code"] != "mount_in_use" {
		t.Fatal(res.Status, got)
	}
}

func TestMountDeletionErrorsHaveSpecificRecoveryCodes(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	source := &archiveActionFixture{fakeBackend: b}
	h.backend = source
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{{manager.ErrMountUnsynced, "mount_unsynced", 409}, {manager.ErrHistoryPending, "history_pending", 409}, {manager.ErrPending, "mount_in_use", 409}, {manager.ErrCleanupPending, "cleanup_pending", 503}} {
		source.failure = tc.err
		res := request(t, c, h, "POST", "/api/mounts/"+uuid.NewString()+"/delete", map[string]any{}, csrf, h.URL())
		var got map[string]any
		json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if res.StatusCode != tc.status || got["code"] != tc.code || got["message"] == "" {
			t.Fatalf("missing recovery guidance: %s %+v", res.Status, got)
		}
	}
}
