package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ownedlogin"
)

type fakeDiscoveryBackend struct {
	*fakeBackend
	discoveryMu sync.Mutex
	view        manager.DiscoveryView
	actions     []string
	input       string
	err         error
}

func (b *fakeDiscoveryBackend) DiscoveryViews(context.Context) ([]manager.DiscoveryView, error) {
	b.discoveryMu.Lock()
	defer b.discoveryMu.Unlock()
	return []manager.DiscoveryView{b.view}, b.err
}
func (b *fakeDiscoveryBackend) act(action string, id model.ProfileID, input string) (manager.DiscoveryView, error) {
	b.discoveryMu.Lock()
	defer b.discoveryMu.Unlock()
	b.actions = append(b.actions, action+":"+string(id))
	b.input = input
	return b.view, b.err
}
func (b *fakeDiscoveryBackend) StartDiscoveryLogin(_ context.Context, id model.ProfileID, origin string) (manager.DiscoveryView, error) {
	return b.act("login", id, origin)
}
func (b *fakeDiscoveryBackend) CompleteDiscoveryLogin(_ context.Context, id model.ProfileID) (manager.DiscoveryView, error) {
	return b.act("complete", id, "")
}
func (b *fakeDiscoveryBackend) RescanDiscovery(_ context.Context, id model.ProfileID) (manager.DiscoveryView, error) {
	return b.act("rescan", id, "")
}
func (b *fakeDiscoveryBackend) AddDiscoveryFolder(_ context.Context, id model.ProfileID, input string) (manager.DiscoveryView, error) {
	return b.act("add", id, input)
}
func (b *fakeDiscoveryBackend) SearchDiscoveryFolders(_ context.Context, id model.ProfileID, query string) ([]manager.DiscoveryCandidate, error) {
	_, err := b.act("search", id, query)
	return []manager.DiscoveryCandidate{{Name: "团队资料", Token: "folderToken", URL: "javascript:secret"}}, err
}

func TestDiscoveryReadsAreCachedAndErrorsAreSanitized(t *testing.T) {
	h, original, client, csrf, _ := fixture(t)
	id := model.ProfileID(uuid.NewString())
	b := &fakeDiscoveryBackend{fakeBackend: original, view: manager.DiscoveryView{ProfileID: id, State: "browser_disconnected", Error: "raw-secret", Browser: &manager.BrowserBridgeView{ExtensionID: "mpcbleimmgigonbhigohceapnjjcfhec", ExtensionPath: "/Applications/DriveBridge/extension/drivebridge", HelperAvailable: true}, Snapshot: discovery.Snapshot{Binding: discovery.Binding{UserID: "private-user", IdentityHash: "private-hash"}, Sources: map[string]discovery.SourceState{discovery.SharedHidden: {Status: "partial", Error: "raw-secret"}}, Entries: []discovery.Entry{{Token: "folderToken", Name: "团队资料", Memberships: []string{discovery.SharedHidden, discovery.Manual}}}}}}
	h.backend = b
	for i := 0; i < 2; i++ {
		response := request(t, client, h, "GET", "/api/discovery", nil, "", "")
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 || strings.Contains(string(body), "raw-secret") || strings.Contains(string(body), "private-") || !strings.Contains(string(body), "shared_hidden") || !strings.Contains(string(body), "browser_disconnected") || !strings.Contains(string(body), "helper_available") || !strings.Contains(string(body), "/Applications/DriveBridge/extension/drivebridge") {
			t.Fatalf("unsafe or failed read: %s %v", body, err)
		}
	}
	if len(b.actions) != 0 || b.view.Snapshot.Sources[discovery.SharedHidden].Error != "raw-secret" {
		t.Fatal("GET mutated backend state", b.actions, b.view)
	}
	b.err = errors.New("CLI output raw-secret")
	response := request(t, client, h, "POST", "/api/discovery/"+string(id)+"/rescan", map[string]string{}, csrf, h.URL())
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 503 || strings.Contains(string(body), "raw-secret") || !strings.Contains(string(body), "discovery_unavailable") {
		t.Fatalf("unsafe failure: %d %s", response.StatusCode, body)
	}
}

func TestDiscoveryActionsValidateAndPreserveSessionProtection(t *testing.T) {
	h, original, client, csrf, _ := fixture(t)
	id := uuid.NewString()
	b := &fakeDiscoveryBackend{fakeBackend: original, view: manager.DiscoveryView{ProfileID: model.ProfileID(id), State: "needs_login"}}
	h.backend = b
	for _, tc := range []struct {
		action string
		body   map[string]string
	}{
		{"login", map[string]string{"origin": "https://tenant.feishu.cn"}},
		{"complete", map[string]string{}},
		{"rescan", map[string]string{}},
		{"search", map[string]string{"input": "团队"}},
		{"add", map[string]string{"input": "https://tenant.feishu.cn/drive/folder/folderToken"}},
	} {
		response := request(t, client, h, "POST", "/api/discovery/"+id+"/"+tc.action, tc.body, csrf, h.URL())
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || strings.Contains(string(body), "javascript:") {
			t.Fatalf("%s: %d %s", tc.action, response.StatusCode, body)
		}
		if b.actions[len(b.actions)-1] != tc.action+":"+id {
			t.Fatal("wrong profile/action", b.actions)
		}
	}
	called := len(b.actions)
	for _, tc := range []struct {
		path, method, token, origin string
		body                        map[string]string
		status                      int
	}{
		{"/api/discovery/" + id + "/rescan", "POST", "", h.URL(), map[string]string{}, 403},
		{"/api/discovery/" + id + "/rescan", "POST", csrf, "https://foreign.example", map[string]string{}, 403},
		{"/api/discovery/invalid/rescan", "POST", csrf, h.URL(), map[string]string{}, 400},
		{"/api/discovery/" + id + "/rename", "POST", csrf, h.URL(), map[string]string{}, 400},
		{"/api/discovery/" + id + "/search", "POST", csrf, h.URL(), map[string]string{"input": " "}, 400},
		{"/api/discovery/" + id + "/search", "POST", csrf, h.URL(), map[string]string{"input": strings.Repeat("字", 31)}, 400},
		{"/api/discovery/" + id + "/add", "POST", csrf, h.URL(), map[string]string{"input": "x", "name": "override"}, 400},
		{"/api/discovery/" + id + "/login", "POST", csrf, h.URL(), map[string]string{"origin": "https://tenant.feishu.cn/path"}, 400},
		{"/api/discovery/" + id + "/rescan", "POST", csrf, h.URL(), map[string]string{"input": "unexpected"}, 400},
		{"/api/discovery/" + id + "/add", "POST", csrf, h.URL(), map[string]string{"input": strings.Repeat("x", 4097)}, 400},
		{"/api/discovery/" + id + "/complete", "GET", "", "", nil, 405},
		{"/api/discovery", "POST", csrf, h.URL(), map[string]string{}, 405},
	} {
		response := request(t, client, h, tc.method, tc.path, tc.body, tc.token, tc.origin)
		response.Body.Close()
		if response.StatusCode != tc.status {
			t.Fatalf("%s %s: %d, want %d", tc.method, tc.path, response.StatusCode, tc.status)
		}
	}
	if len(b.actions) != called {
		t.Fatal("invalid request reached backend", b.actions)
	}
}

func TestDiscoveryOptionalCapabilityAndAssets(t *testing.T) {
	h, _, client, _, _ := fixture(t)
	response := request(t, client, h, "GET", "/api/discovery", nil, "", "")
	var body map[string]any
	err := json.NewDecoder(response.Body).Decode(&body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || body["code"] != "unavailable" {
		t.Fatal(response.StatusCode, body, err)
	}
	for _, route := range []string{"/", "/discovery-view.js"} {
		response = request(t, client, h, "GET", route, nil, "", "")
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || !strings.Contains(string(raw), "discovery") {
			t.Fatal("missing discovery interface", route, response.StatusCode)
		}
	}
}

func TestManualFolderUnclassifiedIsExplicitAndSanitized(t *testing.T) {
	h, original, client, csrf, _ := fixture(t)
	id := uuid.NewString()
	h.backend = &fakeDiscoveryBackend{fakeBackend: original, err: errors.Join(manager.ErrDiscoveryUnclassified, errors.New("raw-private-proof"))}
	response := request(t, client, h, "POST", "/api/discovery/"+id+"/add", map[string]string{"input": "folderToken"}, csrf, h.URL())
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 409 || !strings.Contains(string(body), "discovery_unclassified") || strings.Contains(string(body), "raw-private-proof") {
		t.Fatalf("unclassified response: %d %s", response.StatusCode, body)
	}
}

func TestDiscoveryMissingRuntimeIsActionable(t *testing.T) {
	h, original, client, csrf, _ := fixture(t)
	id := model.ProfileID(uuid.NewString())
	h.backend = &fakeDiscoveryBackend{fakeBackend: original, err: ownedlogin.ErrRuntimeUnavailable}
	res := request(t, client, h, "POST", "/api/discovery/"+string(id)+"/login", map[string]string{}, csrf, h.URL())
	defer res.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 503 || result["code"] != "needs_runtime" || !strings.Contains(result["message"].(string), "WebView2") {
		t.Fatal(res.Status, result)
	}
	if got := publicAutomaticLogin(manager.AutomaticLoginView{State: "failed", Error: "needs_runtime"}); got.Error != "needs_runtime" {
		t.Fatal(got)
	}
	if got := publicDiscovery(manager.DiscoveryView{State: "needs_runtime", Error: "needs_runtime"}); got.State != "needs_runtime" || got.Error != "needs_runtime" {
		t.Fatal(got)
	}
}
