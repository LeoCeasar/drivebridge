package ui

import (
	"context"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"net/http"
	"testing"
)

type directoryFixture struct {
	*fakeBackend
	paths []string
	ids   []model.MountID
}

func (b *directoryFixture) RefreshDirectory(_ context.Context, id model.MountID, remote string) error {
	b.paths = append(b.paths, remote)
	b.ids = append(b.ids, id)
	return nil
}

func TestDirectoryRefreshEndpointProtectsScopeAndSession(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	source := &directoryFixture{fakeBackend: b}
	h.backend = source
	id := uuid.NewString()
	route := "/api/mounts/" + id + "/refresh-directory"
	for name, tc := range map[string]struct {
		method       string
		body         any
		csrf, origin string
		status       int
	}{
		"scoped":           {"POST", map[string]any{"remote": "个人目录/开发项目文档"}, csrf, h.URL(), 200},
		"root":             {"POST", map[string]any{"remote": ""}, csrf, h.URL(), 200},
		"missing csrf":     {"POST", map[string]any{"remote": ""}, "", h.URL(), 403},
		"foreign origin":   {"POST", map[string]any{"remote": ""}, csrf, "http://evil.example", 403},
		"get":              {"GET", nil, "", "", 405},
		"absolute":         {"POST", map[string]any{"remote": "/tmp"}, csrf, h.URL(), 400},
		"windows absolute": {"POST", map[string]any{"remote": "C:/Users"}, csrf, h.URL(), 400},
		"traversal":        {"POST", map[string]any{"remote": "../x"}, csrf, h.URL(), 400},
		"missing scope":    {"POST", map[string]any{}, csrf, h.URL(), 400},
		"null scope":       {"POST", map[string]any{"remote": nil}, csrf, h.URL(), 400},
		"unknown field":    {"POST", map[string]any{"remote": "a", "force": true}, csrf, h.URL(), 400},
	} {
		res := request(t, c, h, tc.method, route, tc.body, tc.csrf, tc.origin)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%s: %s", name, res.Status)
		}
	}
	if len(source.paths) != 2 {
		t.Fatal("invalid scope reached backend", source.paths)
	}
	res := request(t, &http.Client{}, h, "POST", route, map[string]any{"remote": ""}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
	h.backend = b
	res = request(t, c, h, "POST", route, map[string]any{"remote": ""}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatal("optional backend ignored", res.Status)
	}
}
