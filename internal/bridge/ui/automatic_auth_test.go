package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
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

type startBackendFixture struct {
	*automaticBackendFixture
	flow auth.AuthFlow
	err  error
}

func (b *startBackendFixture) AuthStart(context.Context, model.ProfileID) (auth.AuthFlow, error) {
	return b.flow, b.err
}

func postAuthStart(t *testing.T, flow auth.AuthFlow, startErr error) (int, map[string]any) {
	t.Helper()
	h, b, c, csrf, _ := fixture(t)
	h.backend = &startBackendFixture{automaticBackendFixture: &automaticBackendFixture{fakeBackend: b}, flow: flow, err: startErr}
	res := request(t, c, h, "POST", "/api/auth/start", map[string]string{"profile_id": uuid.NewString()}, csrf, h.URL())
	defer res.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, result
}

// Regression: an interrupted authorization (expired QR code + restart) used to
// answer a bare 503 "offline". Failures after reconcile need a specific code.
func TestAutomaticAuthStartReconcileFailureHasSpecificCode(t *testing.T) {
	for name, err := range map[string]error{
		"wrapped": errors.Join(auth.ErrReconcileFailed, auth.ErrNeedsReconcile, errors.New("token=secret")),
		"bare":    auth.ErrNeedsReconcile,
	} {
		t.Run(name, func(t *testing.T) {
			status, body := postAuthStart(t, auth.AuthFlow{}, err)
			if status != http.StatusServiceUnavailable || body["code"] != "auth_reconcile_failed" || body["retryable"] != true {
				t.Fatal(status, body)
			}
			message, _ := body["message"].(string)
			if message == "" || strings.Contains(message, "secret") || body["code"] == "offline" {
				t.Fatal(body)
			}
		})
	}
}
func TestAutomaticAuthStartIdentityMismatchAndBusyKeepMeaning(t *testing.T) {
	status, body := postAuthStart(t, auth.AuthFlow{}, errors.Join(auth.ErrReconcileFailed, model.ErrIdentityChanged))
	if status != http.StatusConflict || body["code"] != "auth_identity_changed" || body["retryable"] != false || body["message"] == "" {
		t.Fatal(status, body)
	}
	status, body = postAuthStart(t, auth.AuthFlow{}, errors.Join(auth.ErrReconcileFailed, auth.ErrBusy))
	if status != http.StatusServiceUnavailable || body["code"] != "local_busy" {
		t.Fatal(status, body)
	}
}
func TestAutomaticAuthStartReconciledFlowHasNoLinkButIsTracked(t *testing.T) {
	id := uuid.NewString()
	status, body := postAuthStart(t, auth.AuthFlow{ID: id, Kind: "reconciled"}, nil)
	if status != http.StatusOK || body["id"] != id || body["kind"] != "reconciled" || body["url"] != "" || body["auto_complete"] != true {
		t.Fatal(status, body)
	}
	// Only the reconciled kind may omit the link, and it may never carry an unsafe one.
	if status, body = postAuthStart(t, auth.AuthFlow{ID: id, Kind: "user_auth"}, nil); status != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatal(status, body)
	}
	if status, body = postAuthStart(t, auth.AuthFlow{ID: id, Kind: "reconciled", URL: "javascript:secret"}, nil); status != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatal(status, body)
	}
}
func TestAutomaticAuthStatusPassesReconcileFailureCode(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	id := uuid.NewString()
	h.backend = &automaticBackendFixture{fakeBackend: b, progress: manager.AutomaticLoginView{FlowID: id, State: "failed", Error: "auth_reconcile_failed"}}
	res := request(t, c, h, "POST", "/api/auth/status", map[string]string{"flow_id": id}, csrf, h.URL())
	defer res.Body.Close()
	var result map[string]any
	json.NewDecoder(res.Body).Decode(&result)
	if res.StatusCode != http.StatusOK || result["error"] != "auth_reconcile_failed" || result["state"] != "failed" {
		t.Fatal(res.Status, result)
	}
}

type progressMountBackend struct {
	*fakeBackend
	mounts []MountView
}

func (b progressMountBackend) Status(context.Context) (StatusView, error) {
	return StatusView{Mounts: b.mounts}, nil
}
func (b progressMountBackend) Config(context.Context) (ConfigView, error) {
	return ConfigView{Mounts: b.mounts}, nil
}
func TestMountAuthorizationProjectionIsScopedAndSanitized(t *testing.T) {
	for _, route := range []string{"/api/status", "/api/config"} {
		t.Run(route, func(t *testing.T) {
			h, b, c, _, _ := fixture(t)
			profile := model.ProfileID(uuid.NewString())
			progress := manager.AutomaticLoginView{ProfileID: profile, State: "waiting_authorization", Flow: auth.AuthFlow{URL: "https://accounts.feishu.cn/consent"}}
			unsafe := progress
			unsafe.Flow.URL = "javascript:SECRET"
			unsafe.Error = "SECRET"
			failed := progress
			failed.State = "failed"
			failed.Error = "expired"
			h.backend = progressMountBackend{b, []MountView{
				{ID: "waiting", ProfileID: profile, Authorization: &progress},
				{ID: "other", ProfileID: "other", Authorization: &progress},
				{ID: "unsafe", ProfileID: profile, Authorization: &unsafe},
				{ID: "failed", ProfileID: profile, Authorization: &failed},
			}}
			res := request(t, c, h, "GET", route, nil, "", "")
			defer res.Body.Close()
			var body struct {
				Mounts []MountView `json:"mounts"`
			}
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Mounts) != 4 {
				t.Fatal(body)
			}
			if body.Mounts[0].Authorization == nil || body.Mounts[0].Authorization.Flow.URL != progress.Flow.URL {
				t.Fatal("new tabs lost current authorization")
			}
			if body.Mounts[1].Authorization != nil {
				t.Fatal("another account received this flow")
			}
			if body.Mounts[2].Authorization.Flow.URL != "" || body.Mounts[2].Authorization.Error != "operation_failed" {
				t.Fatal("unsafe progress exposed")
			}
			if body.Mounts[3].Authorization.Flow.URL != "" {
				t.Fatal("finished flow exposed stale consent link")
			}
			if unsafe.Flow.URL != "javascript:SECRET" {
				t.Fatal("shared backend data mutated")
			}
		})
	}
}

func TestConfigEmptyMountsRemainJSONArray(t *testing.T) {
	h, _, client, _, _ := fixture(t)
	res := request(t, client, h, "GET", "/api/config", nil, "", "")
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["mounts"].([]any); !ok {
		t.Fatal("first-run mount list must remain an array for browser startup", body["mounts"])
	}
}
