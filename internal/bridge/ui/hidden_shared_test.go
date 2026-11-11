package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

type hiddenSharedBackend struct {
	*fakeDiscoveryBackend
	ids    []model.ProfileID
	values []bool
}

func (b *hiddenSharedBackend) SetHiddenSharedSync(_ context.Context, id model.ProfileID, enabled bool) (manager.DiscoveryView, error) {
	b.ids = append(b.ids, id)
	b.values = append(b.values, enabled)
	b.view.SyncHiddenShared = enabled
	return b.view, b.err
}
func TestHiddenSharedSyncRequestPreservesFalseAndAccount(t *testing.T) {
	h, original, c, csrf, _ := fixture(t)
	b := &hiddenSharedBackend{fakeDiscoveryBackend: &fakeDiscoveryBackend{fakeBackend: original}}
	h.backend = b
	for _, enabled := range []bool{false, true} {
		id := model.ProfileID(uuid.NewString())
		b.view.ProfileID = id
		res := request(t, c, h, "POST", "/api/discovery/"+string(id)+"/hidden-sync", map[string]any{"enabled": enabled}, csrf, h.URL())
		var got manager.DiscoveryView
		err := json.NewDecoder(res.Body).Decode(&got)
		res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK || got.ProfileID != id || got.SyncHiddenShared != enabled || len(b.ids) == 0 || b.ids[len(b.ids)-1] != id || b.values[len(b.values)-1] != enabled {
			t.Fatal(res.Status, got, b.ids, b.values, err)
		}
	}
}
func TestHiddenSharedSyncValidationAndSession(t *testing.T) {
	h, original, c, csrf, _ := fixture(t)
	b := &hiddenSharedBackend{fakeDiscoveryBackend: &fakeDiscoveryBackend{fakeBackend: original}}
	h.backend = b
	id := uuid.NewString()
	route := "/api/discovery/" + id + "/hidden-sync"
	for _, tc := range []struct {
		method, path, token, origin string
		body                        any
		want                        int
	}{
		{"POST", route, csrf, h.URL(), map[string]any{}, 400},
		{"POST", route, csrf, h.URL(), map[string]any{"enabled": nil}, 400},
		{"POST", route, csrf, h.URL(), map[string]any{"enabled": "false"}, 400},
		{"POST", route, csrf, h.URL(), map[string]any{"enabled": false, "input": "folder"}, 400},
		{"POST", "/api/discovery/invalid/hidden-sync", csrf, h.URL(), map[string]any{"enabled": false}, 400},
		{"GET", route, "", "", nil, 405},
		{"POST", route, "", h.URL(), map[string]any{"enabled": false}, 403},
		{"POST", route, csrf, "https://foreign.example", map[string]any{"enabled": false}, 403},
	} {
		res := request(t, c, h, tc.method, tc.path, tc.body, tc.token, tc.origin)
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("%s %s: %d want %d", tc.method, tc.path, res.StatusCode, tc.want)
		}
	}
	if len(b.ids) != 0 {
		t.Fatal("invalid request reached backend", b.ids)
	}
	h.backend = &fakeDiscoveryBackend{fakeBackend: original}
	res := request(t, c, h, "POST", route, map[string]any{"enabled": false}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatal("old backend capability", res.Status)
	}
}

func TestHiddenSharedSyncFailureIsDirectorySettingFeedback(t *testing.T) {
	h, original, c, csrf, _ := fixture(t)
	b := &hiddenSharedBackend{fakeDiscoveryBackend: &fakeDiscoveryBackend{fakeBackend: original, err: errors.New("private helper output")}}
	h.backend = b
	res := request(t, c, h, "POST", "/api/discovery/"+uuid.NewString()+"/hidden-sync", map[string]any{"enabled": false}, csrf, h.URL())
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 503 || !strings.Contains(string(body), "discovery_unavailable") || strings.Contains(string(body), "private") || strings.Contains(string(body), "草稿") {
		t.Fatal(res.Status, string(body))
	}
}
