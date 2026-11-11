package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/manager"
)

type automaticBackendFixture struct {
	*fakeBackend
	progress manager.AutomaticLoginView
	queries  int
}

func (b *automaticBackendFixture) AuthProgress(context.Context, string) (manager.AutomaticLoginView, error) {
	b.queries++
	return b.progress, nil
}
func TestAutomaticAuthStartAdvertisesServerOwnedCompletion(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	automatic := &automaticBackendFixture{fakeBackend: b}
	h.backend = automatic
	res := request(t, c, h, "POST", "/api/auth/start", map[string]string{"profile_id": uuid.NewString()}, csrf, h.URL())
	defer res.Body.Close()
	var result map[string]any
	json.NewDecoder(res.Body).Decode(&result)
	if res.StatusCode != 200 || result["auto_complete"] != true {
		t.Fatal(res.Status, result)
	}
	if automatic.queries != 0 {
		t.Fatal("starting a flow unexpectedly queried or completed it")
	}
}
func TestAutomaticAuthStatusIsProtectedAndSanitized(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	id := uuid.NewString()
	automatic := &automaticBackendFixture{fakeBackend: b, progress: manager.AutomaticLoginView{FlowID: id, State: "failed", Error: "raw CLI secret"}}
	h.backend = automatic
	res := request(t, c, h, "POST", "/api/auth/status", map[string]string{"flow_id": id}, csrf, h.URL())
	var result map[string]any
	json.NewDecoder(res.Body).Decode(&result)
	res.Body.Close()
	if res.StatusCode != 200 || result["error"] != "operation_failed" || strings.Contains(result["error"].(string), "secret") {
		t.Fatal(res.Status, result)
	}
	for _, tc := range []struct {
		method, token, id string
		status            int
	}{{"GET", csrf, id, 405}, {"POST", "", id, 403}, {"POST", csrf, "invalid", 400}} {
		res = request(t, c, h, tc.method, "/api/auth/status", map[string]string{"flow_id": tc.id}, tc.token, h.URL())
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatal(res.Status, tc)
		}
	}
	if automatic.queries != 1 {
		t.Fatal("invalid requests reached auth progress backend", automatic.queries)
	}
}
func TestAutomaticAuthRejectsUnsafeProgressLink(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	id := uuid.NewString()
	automatic := &automaticBackendFixture{fakeBackend: b, progress: manager.AutomaticLoginView{FlowID: id, State: "waiting_authorization"}}
	automatic.progress.Flow.URL = "javascript:secret"
	h.backend = automatic
	res := request(t, c, h, "POST", "/api/auth/status", map[string]string{"flow_id": id}, csrf, h.URL())
	defer res.Body.Close()
	var result manager.AutomaticLoginView
	json.NewDecoder(res.Body).Decode(&result)
	if res.StatusCode != http.StatusOK || result.Flow.URL != "" || result.State != "failed" {
		t.Fatal(res.Status, result)
	}
}
