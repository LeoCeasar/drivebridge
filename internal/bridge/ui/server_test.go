package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
)

var ctx = context.Background()

type fakeBackend struct {
	mu                           sync.Mutex
	input                        ConflictInputs
	draftFile                    string
	resolveErr                   error
	authCompleteErr              error
	lastConfig                   ConfigRequest
	resolves, starts, configured int
}

func (b *fakeBackend) Status(context.Context) (StatusView, error) {
	return StatusView{SchemaVersion: 1, Conflicts: []ConflictSummary{{ID: b.input.Conflict.ID, Name: "设计.md"}}}, nil
}
func (b *fakeBackend) Config(context.Context) (ConfigView, error) {
	return ConfigView{Profiles: []ProfileView{}, Mounts: []MountView{}}, nil
}
func (b *fakeBackend) Configure(_ context.Context, r ConfigRequest) (ConfigView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.configured++
	b.lastConfig = r
	return ConfigView{Profiles: []ProfileView{{Name: r.Name, Brand: r.Brand}}}, nil
}
func TestMountConfigurationAcceptsExplicitMacOSBackend(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	res := request(t, c, h, "POST", "/api/config", map[string]string{"action": "mount", "name": "FSKit test", "remote_id": uuid.NewString(), "mountpoint": "/tmp/bridge-test", "macos_backend": "fskit"}, csrf, h.URL())
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatal(res.Status)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	raw, err := json.Marshal(b.lastConfig)
	if err != nil || !bytes.Contains(raw, []byte(`"macos_backend":"fskit"`)) {
		t.Fatalf("explicit backend did not reach configuration adapter: %s, %v", raw, err)
	}
}
func (b *fakeBackend) AuthStart(context.Context, model.ProfileID) (auth.AuthFlow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	return auth.AuthFlow{ID: uuid.NewString(), URL: "https://accounts.example/authorize"}, nil
}
func (b *fakeBackend) AuthComplete(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.authCompleteErr
}
func (b *fakeBackend) Conflict(_ context.Context, id model.ConflictID) (ConflictInputs, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != b.input.Conflict.ID {
		return ConflictInputs{}, os.ErrNotExist
	}
	v := b.input
	v.Draft, _ = os.ReadFile(b.draftFile)
	return v, nil
}
func (b *fakeBackend) SaveDraft(_ context.Context, id model.ConflictID, v []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != b.input.Conflict.ID {
		return os.ErrNotExist
	}
	return os.WriteFile(b.draftFile, v, 0600)
}
func (b *fakeBackend) Resolve(_ context.Context, r model.ResolveRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resolves++
	saved, _ := os.ReadFile(b.draftFile)
	if !bytes.Equal(saved, r.Content) {
		return errors.New("draft was not saved first")
	}
	return b.resolveErr
}
func (b *fakeBackend) Mount(context.Context, model.MountID, string) error { return nil }

type fakeNotifier struct {
	mu              sync.Mutex
	desktop         bool
	notifies, opens int
}

func (n *fakeNotifier) Available() bool { return n.desktop }
func (n *fakeNotifier) Notify(context.Context, string, string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notifies++
	return nil
}
func (n *fakeNotifier) Open(context.Context, string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.opens++
	return nil
}
func fixture(t *testing.T) (*Host, *fakeBackend, *http.Client, string, Options) {
	t.Helper()
	d, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	id := model.ConflictID(uuid.NewString())
	entry := model.EntryID(uuid.NewString())
	b := &fakeBackend{draftFile: filepath.Join(d, "draft"), input: ConflictInputs{Conflict: model.Conflict{ID: id, EntryID: entry, Generation: 4, RemoteRevision: "r1"}, Entry: model.Entry{ID: entry, Remote: "设计.md", Generation: 4}, Base: []byte("base\n"), Local: []byte("local\n"), Remote: []byte("remote\n"), Text: true}}
	o := Options{Backend: b, RuntimeDir: filepath.Join(d, "ui"), ConfigKey: "fixture"}
	h, e := Start(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close(ctx) })
	client, csrf := bootstrap(t, h)
	return h, b, client, csrf, o
}
func bootstrap(t *testing.T, h *Host) (*http.Client, string) {
	t.Helper()
	raw, e := h.BootstrapURL("")
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(raw)
	token := strings.TrimPrefix(u.Fragment, "bootstrap=")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	response := request(t, c, h, "POST", "/api/session", map[string]string{"bootstrap": token}, "", h.URL())
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	defer response.Body.Close()
	var v struct {
		CSRF string `json:"csrf"`
	}
	if e = json.NewDecoder(response.Body).Decode(&v); e != nil || v.CSRF == "" {
		t.Fatal(v, e)
	}
	for _, cookie := range response.Cookies() {
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatal("unsafe cookie", cookie)
		}
	}
	return c, v.CSRF
}
func request(t *testing.T, c *http.Client, h *Host, method, path string, data any, csrf, origin string) *http.Response {
	t.Helper()
	var body io.Reader
	if data != nil {
		b, _ := json.Marshal(data)
		body = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, h.URL()+path, body)
	if e != nil {
		t.Fatal(e)
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	res, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	return res
}
func TestFirstRunWithoutMountWorker(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	res := request(t, c, h, "GET", "/api/status", nil, "", "")
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", "/api/config", ConfigRequest{Action: "profile", Name: "我的飞书", Brand: "feishu"}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || b.configured != 1 {
		t.Fatal(res.Status, b.configured)
	}
	res = request(t, c, h, "POST", "/api/auth/start", map[string]string{"profile_id": uuid.NewString()}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || b.starts != 1 {
		t.Fatal(res.Status, b.starts)
	}
}
func TestConflictInputsChangedReturns409(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	b.resolveErr = model.ErrVersionChanged
	r := model.ResolveRequest{ConflictID: b.input.Conflict.ID, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1", Content: []byte("saved merge")}
	res := request(t, c, h, "POST", "/api/conflicts/"+string(r.ConflictID)+"/resolve", r, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 409 || b.resolves != 1 {
		t.Fatal(res.Status, b.resolves)
	}
	saved, _ := os.ReadFile(b.draftFile)
	if string(saved) != "saved merge" {
		t.Fatal(string(saved))
	}
}
func TestDraftSurvivesCloseAndRestart(t *testing.T) {
	h, b, c, csrf, o := fixture(t)
	id := string(b.input.Conflict.ID)
	res := request(t, c, h, "PUT", "/api/conflicts/"+id+"/draft", map[string][]byte{"content": []byte("persistent draft")}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	h.Close(ctx)
	next, e := Start(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close(ctx)
	c, _ = bootstrap(t, next)
	res = request(t, c, next, "GET", "/api/conflicts/"+id, nil, "", "")
	defer res.Body.Close()
	var v ConflictView
	if e = json.NewDecoder(res.Body).Decode(&v); e != nil || v.Draft != "persistent draft" {
		t.Fatal(v, e)
	}
}
func TestCSRFAndForeignOriginDenied(t *testing.T) {
	h, _, c, csrf, _ := fixture(t)
	for _, x := range []struct{ origin, csrf string }{{"https://foreign.example", csrf}, {h.URL(), ""}, {"", csrf}} {
		res := request(t, c, h, "POST", "/api/auth/start", map[string]string{"profile_id": uuid.NewString()}, x.csrf, x.origin)
		res.Body.Close()
		if res.StatusCode != 403 {
			t.Fatal(res.Status)
		}
	}
	req, _ := http.NewRequest("GET", h.URL()+"/api/status", nil)
	req.Host = "evil.example"
	res, e := c.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
}
func TestPathTraversalDenied(t *testing.T) {
	h, _, c, _, _ := fixture(t)
	for _, p := range []string{"/../go.mod", "/assets/%2e%2e/server.go", "/api/conflicts/..%2Fother", "//api/status"} {
		res := request(t, c, h, "GET", p, nil, "", "")
		res.Body.Close()
		if res.StatusCode != 400 && res.StatusCode != 404 {
			t.Fatal(p, res.Status)
		}
	}
}
func TestConflictHTMLIsEscaped(t *testing.T) {
	h, b, c, _, _ := fixture(t)
	b.input.Local = []byte(`<script>alert("x")</script>`)
	res := request(t, c, h, "GET", "/api/conflicts/"+string(b.input.Conflict.ID), nil, "", "")
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if bytes.Contains(raw, []byte("<script>")) {
		t.Fatal(string(raw))
	}
	var v ConflictView
	if e := json.Unmarshal(raw, &v); e != nil || v.Local != string(b.input.Local) {
		t.Fatal(v, e)
	}
	asset, e := assets.ReadFile("assets/app.js")
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(asset, []byte("innerHTML")) {
		t.Fatal("unsafe HTML rendering")
	}
}
func TestNotifyDeduplicates(t *testing.T) {
	h, b, _, _, _ := fixture(t)
	n := &fakeNotifier{desktop: true}
	h.notifier = n
	for i := 0; i < 2; i++ {
		if e := h.NotifyConflict(ctx, b.input.Conflict.ID); e != nil {
			t.Fatal(e)
		}
	}
	b.input.Conflict.RemoteRevision = "r2"
	if e := h.NotifyConflict(ctx, b.input.Conflict.ID); e != nil {
		t.Fatal(e)
	}
	if n.opens != 1 || n.notifies != 1 {
		t.Fatal(n.opens, n.notifies)
	}
}
func TestNoDesktopStillKeepsConflict(t *testing.T) {
	h, b, _, _, _ := fixture(t)
	n := &fakeNotifier{}
	h.notifier = n
	if e := h.NotifyConflict(ctx, b.input.Conflict.ID); e != nil {
		t.Fatal(e)
	}
	if n.opens != 0 || n.notifies != 0 {
		t.Fatal(n)
	}
	if _, e := b.Conflict(ctx, b.input.Conflict.ID); e != nil {
		t.Fatal(e)
	}
}
func TestOneTimeBootstrapAndSingleInstanceControl(t *testing.T) {
	h, _, c, _, o := fixture(t)
	if second, e := Start(ctx, o); !errors.Is(e, ErrAlreadyRunning) {
		if second != nil {
			second.Close(ctx)
		}
		t.Fatal(e)
	}
	raw, e := OpenExisting(ctx, o.RuntimeDir, o.ConfigKey, "")
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(raw)
	token := strings.TrimPrefix(u.Fragment, "bootstrap=")
	data := map[string]string{"bootstrap": token}
	res := request(t, c, h, "POST", "/api/session", data, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", "/api/session", data, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("bootstrap replay", res.Status)
	}
}

func TestEmptyDraftIsDistinctFromNoDraft(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	res := request(t, c, h, "PUT", "/api/conflicts/"+string(b.input.Conflict.ID)+"/draft", map[string][]byte{"content": {}}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "GET", "/api/conflicts/"+string(b.input.Conflict.ID), nil, "", "")
	defer res.Body.Close()
	var raw map[string]any
	if e := json.NewDecoder(res.Body).Decode(&raw); e != nil {
		t.Fatal(e)
	}
	if raw["has_draft"] != true {
		t.Fatal("empty draft would be replaced by local source", raw)
	}
}
func TestStatusNeverReturnsCredentialLikeErrorText(t *testing.T) {
	if safeCode("request_timeout") != "request_timeout" {
		t.Fatal("safe timeout reason lost")
	}
	if got := safeCode("secretlowercasebearertoken"); got == "secretlowercasebearertoken" {
		t.Fatal("untrusted status error passed through")
	}
}
func TestDiffHunksPreserveIndependentAndInsertedChanges(t *testing.T) {
	hunks := diffHunks("one\ntwo\nthree\n", "ONE\ntwo\nthree\n", "one\ntwo\nTHREE\n")
	if len(hunks) != 2 || hunks[0].Local != "ONE\n" || hunks[0].Remote != "one\n" || hunks[1].Remote != "THREE\n" {
		t.Fatal(hunks)
	}
	insert := diffHunks("a\n", "new\na\n", "a\n")
	if len(insert) != 1 || insert[0].BaseStart != 0 || insert[0].BaseEnd != 0 || insert[0].Local != "new\n" || insert[0].Remote != "" {
		t.Fatal(insert)
	}
}
func TestExpiredBootstrapAndBodyLimits(t *testing.T) {
	h, _, c, _, _ := fixture(t)
	raw, e := h.BootstrapURL("")
	if e != nil {
		t.Fatal(e)
	}
	u, _ := url.Parse(raw)
	token := strings.TrimPrefix(u.Fragment, "bootstrap=")
	h.mu.Lock()
	h.bootstraps[tokenHash(token)] = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	res := request(t, c, h, "POST", "/api/session", map[string]string{"bootstrap": token}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", "/api/auth/start", map[string]string{"profile_id": uuid.NewString(), "secret": "must-not-be-accepted"}, "invalid", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
}

type sharedClaims struct {
	*fakeBackend
	claims   int
	recovery int
	allow    bool
}

func (b *sharedClaims) ClaimConflictNotification(context.Context, model.ConflictID) (bool, error) {
	b.claims++
	return b.allow, nil
}
func (b *sharedClaims) AuthStartRecovery(context.Context, model.ProfileID) (auth.AuthFlow, error) {
	b.recovery++
	return auth.AuthFlow{ID: uuid.NewString(), URL: "https://accounts.example/recover"}, nil
}
func TestNotificationSharesWorkerClaimAndExplicitRecoveryAuth(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	shared := &sharedClaims{fakeBackend: b}
	h.backend = shared
	n := &fakeNotifier{desktop: true}
	h.notifier = n
	if err := h.NotifyConflict(ctx, b.input.Conflict.ID); err != nil {
		t.Fatal(err)
	}
	if shared.claims != 1 || n.opens != 0 || n.notifies != 0 {
		t.Fatal(shared.claims, n)
	}
	res := request(t, c, h, "POST", "/api/auth/start", map[string]any{"profile_id": uuid.NewString(), "recovery": true}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || shared.recovery != 1 || b.starts != 0 {
		t.Fatal(res.Status, shared.recovery, b.starts)
	}
}
func TestNotifyDeduplicationSurvivesHostRestart(t *testing.T) {
	h, b, _, _, o := fixture(t)
	first := &fakeNotifier{desktop: true}
	h.notifier = first
	if e := h.NotifyConflict(ctx, b.input.Conflict.ID); e != nil {
		t.Fatal(e)
	}
	if e := h.Close(ctx); e != nil {
		t.Fatal(e)
	}
	second := &fakeNotifier{desktop: true}
	o.Notifier = second
	next, e := Start(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close(ctx)
	if e = next.NotifyConflict(ctx, b.input.Conflict.ID); e != nil {
		t.Fatal(e)
	}
	if second.opens != 0 || second.notifies != 0 {
		t.Fatal(second)
	}
}
func TestChangedLocalGenerationRetainsDraftBefore409(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	b.input.Entry.Generation = 5
	r := model.ResolveRequest{ConflictID: b.input.Conflict.ID, ExpectedGeneration: 4, ExpectedRemoteRevision: "r1", Content: []byte("do not lose this")}
	res := request(t, c, h, "POST", "/api/conflicts/"+string(r.ConflictID)+"/resolve", r, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 409 || b.resolves != 0 {
		t.Fatal(res.Status, b.resolves)
	}
	saved, _ := os.ReadFile(b.draftFile)
	if string(saved) != "do not lose this" {
		t.Fatal(string(saved))
	}
}
func TestControlSessionMethodAndJSONSecurity(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	res := request(t, c, h, "POST", "/internal/open", map[string]string{"instance": h.instance}, "", h.URL())
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "DELETE", "/api/status", nil, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 405 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "POST", "/api/auth/start", map[string]string{"profile_id": uuid.NewString(), "secret": "not-accepted"}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal(res.Status)
	}
	res = request(t, c, h, "GET", "/api/conflicts/"+string(b.input.Conflict.ID), nil, "", "")
	defer res.Body.Close()
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || res.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(res.Header)
	}
}

func TestNoTextOrNoChangesReturnsUsableEmptyHunkList(t *testing.T) {
	h, b, c, _, _ := fixture(t)
	b.input.Local = b.input.Base
	b.input.Remote = b.input.Base
	for _, text := range []bool{true, false} {
		b.input.Text = text
		res := request(t, c, h, "GET", "/api/conflicts/"+string(b.input.Conflict.ID), nil, "", "")
		var raw map[string]json.RawMessage
		e := json.NewDecoder(res.Body).Decode(&raw)
		res.Body.Close()
		if e != nil {
			t.Fatal(e)
		}
		if string(raw["hunks"]) != "[]" {
			t.Fatalf("empty source broke hunk rendering: %s", raw["hunks"])
		}
	}
}

func TestDiffHunksExposeSourceLineRangesAndActualChanges(t *testing.T) {
	hunks := diffHunks("标题😀\r\n第一行\n第二行\n末行", "标题😀\r\n第一行\n本地插入\n第二行\n末行", "标题😀\r\n第一行\n云端改写\n末行")
	if len(hunks) != 1 {
		t.Fatal(hunks)
	}
	b, _ := json.Marshal(hunks[0])
	var h map[string]any
	json.Unmarshal(b, &h)
	if h["LocalStart"] != float64(2) || h["LocalEnd"] != float64(4) || h["RemoteStart"] != float64(2) || h["RemoteEnd"] != float64(3) {
		t.Fatalf("missing precise source ranges: %s", b)
	}
	changes, ok := h["LocalChanges"].([]any)
	if !ok || len(changes) != 1 {
		t.Fatalf("unchanged local line should remain unhighlighted: %s", b)
	}
	change := changes[0].(map[string]any)
	if change["Kind"] != "insert" || change["Start"] != float64(2) || change["End"] != float64(3) {
		t.Fatal(change)
	}
}

func TestAuthCompleteGuidesUserLoginAfterApplicationConfiguration(t *testing.T) {
	for _, result := range []struct {
		name string
		err  error
	}{
		{"direct", auth.ErrUserMissing},
		{"wrapped", errors.Join(errors.New("complete application setup"), auth.ErrUserMissing)},
	} {
		t.Run(result.name, func(t *testing.T) {
			h, b, client, csrf, _ := fixture(t)
			b.mu.Lock()
			b.authCompleteErr = result.err
			b.mu.Unlock()
			response := request(t, client, h, "POST", "/api/auth/complete", map[string]string{"flow_id": uuid.NewString()}, csrf, h.URL())
			defer response.Body.Close()
			var body struct {
				Code       string `json:"code"`
				Message    string `json:"message"`
				Retryable  bool   `json:"retryable"`
				Authorized *bool  `json:"authorized"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusConflict || body.Code != "user_authorization_required" || body.Message != "应用已配置，请再次点击授权以完成用户登录。" || body.Retryable || body.Authorized != nil {
				t.Fatalf("missing user login reported incorrectly: status=%d body=%+v", response.StatusCode, body)
			}
		})
	}
}
