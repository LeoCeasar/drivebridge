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
}

func (b *archiveActionFixture) Mount(_ context.Context, _ model.MountID, action string) error {
	b.actions = append(b.actions, action)
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
	for _, action := range []string{"archive", "restore"} {
		res := request(t, c, h, "POST", "/api/mounts/"+id+"/"+action, map[string]any{}, csrf, h.URL())
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(res.Status)
		}
	}
	if len(source.actions) != 2 || source.actions[0] != "archive" || source.actions[1] != "restore" {
		t.Fatal(source.actions)
	}
	res := request(t, c, h, "POST", "/api/mounts/"+id+"/archive", map[string]any{}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 || len(source.actions) != 2 {
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
