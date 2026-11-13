package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/manager"
)

type conversionRulesBackend struct {
	*fakeBackend
	view     manager.ConversionRulesView
	err      error
	revision string
	document json.RawMessage
	saves    int
}

func (b *conversionRulesBackend) ConversionRules(context.Context) (manager.ConversionRulesView, error) {
	return b.view, b.err
}
func (b *conversionRulesBackend) SaveConversionRules(_ context.Context, revision string, document json.RawMessage) (manager.ConversionRulesView, error) {
	b.saves++
	b.revision = revision
	b.document = document
	return b.view, b.err
}
func conversionRulesFixture(t *testing.T) (*Host, *conversionRulesBackend, *http.Client, string) {
	t.Helper()
	b := &conversionRulesBackend{fakeBackend: &fakeBackend{}, view: manager.ConversionRulesView{Path: "/private/rules.json", Revision: strings.Repeat("a", 64), Document: json.RawMessage(`{"version":1,"rules":[]}`), RestartRequired: true}}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, err := Start(ctx, Options{Backend: b, RuntimeDir: filepath.Join(directory, "ui"), ConfigKey: "conversion-rules"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(ctx) })
	c, csrf := bootstrap(t, h)
	return h, b, c, csrf
}
func TestConversionRulesAPI(t *testing.T) {
	h, b, c, csrf := conversionRulesFixture(t)
	res := request(t, c, h, "GET", "/api/conversion-rules", nil, "", "")
	defer res.Body.Close()
	var view manager.ConversionRulesView
	if err := json.NewDecoder(res.Body).Decode(&view); err != nil || res.StatusCode != 200 || view.Path != b.view.Path || !view.RestartRequired {
		t.Fatalf("get %d %+v %v", res.StatusCode, view, err)
	}
	res = request(t, c, h, "PUT", "/api/conversion-rules", map[string]any{"revision": b.view.Revision, "document": map[string]any{"version": 1, "rules": []any{}}}, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != 200 || b.saves != 1 || b.revision != b.view.Revision || !json.Valid(b.document) {
		t.Fatalf("put %d %+v", res.StatusCode, b)
	}
	for _, tc := range []struct {
		err     error
		status  int
		message string
	}{{fmt.Errorf("%w: target must be text", manager.ErrConversionRulesInvalid), 400, "target must be text"}, {manager.ErrConversionRulesChanged, 409, "草稿已保留"}} {
		b.err = tc.err
		res = request(t, c, h, "PUT", "/api/conversion-rules", map[string]any{"revision": b.view.Revision, "document": b.view.Document}, csrf, h.URL())
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status || !strings.Contains(string(body), tc.message) {
			t.Fatalf("save error %d %s", res.StatusCode, body)
		}
	}
}
func TestConversionRulesAPIRejectsUnsafeAndMalformedRequests(t *testing.T) {
	h, b, c, csrf := conversionRulesFixture(t)
	for _, tc := range []struct {
		method        string
		body          any
		token, origin string
		status        int
	}{{"PUT", map[string]any{"revision": b.view.Revision, "document": b.view.Document}, "", h.URL(), 403}, {"PUT", map[string]any{"revision": b.view.Revision, "document": b.view.Document}, csrf, "https://foreign.example", 403}, {"PUT", map[string]any{"revision": "", "document": b.view.Document}, csrf, h.URL(), 400}, {"PUT", map[string]any{"revision": "invalid", "document": b.view.Document}, csrf, h.URL(), 400}, {"PUT", map[string]any{"revision": b.view.Revision}, csrf, h.URL(), 400}, {"PUT", map[string]any{"revision": b.view.Revision, "document": nil}, csrf, h.URL(), 400}, {"PUT", map[string]any{"revision": b.view.Revision, "document": b.view.Document, "extra": true}, csrf, h.URL(), 400}, {"POST", nil, csrf, h.URL(), 405}} {
		res := request(t, c, h, tc.method, "/api/conversion-rules", tc.body, tc.token, tc.origin)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%+v: %d", tc, res.StatusCode)
		}
	}
	req, _ := http.NewRequest("PUT", h.URL()+"/api/conversion-rules", strings.NewReader(`{"revision":`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", h.URL())
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Fatal(res.Status)
	}
	res = request(t, &http.Client{}, h, "GET", "/api/conversion-rules", nil, "", "")
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.Status)
	}
	if b.saves != 0 {
		t.Fatalf("invalid request reached backend %d", b.saves)
	}
}
func TestConversionRulesOptionalBackendAndAsset(t *testing.T) {
	h, _, c, _, _ := fixture(t)
	res := request(t, c, h, "GET", "/api/conversion-rules", nil, "", "")
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatal(res.Status)
	}
	res = request(t, &http.Client{}, h, "GET", "/conversion-rules.js", nil, "", "")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "DriveBridgeRules") {
		t.Fatalf("asset %d", res.StatusCode)
	}
}
