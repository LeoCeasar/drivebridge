package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ui"
)

const plainMDToken = "synthetic-ordinary-markdown"
const plainMDName = "ordinary.md"
const plainMDBase = "alpha one\nbeta two\n"

func addPlainMarkdown(h *harness) {
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	h.cloud.resources[plainMDToken] = resource{kind: model.KindFile, name: plainMDName, parent: h.cloud.root, data: []byte(plainMDBase), revision: 1}
}
func TestOrdinaryMarkdownVFSAndHTTPMerge(t *testing.T) {
	h := newHarness(t)
	addPlainMarkdown(h)
	if h.read(plainMDName) != plainMDBase {
		t.Fatal("cold read")
	}
	h.cloud.setFile(plainMDToken, []byte("alpha one\nbeta remote\n"))
	entry := h.save(plainMDName, "alpha local\nbeta two\n")
	if entry.Kind != model.KindFile {
		t.Fatal("ordinary file changed kind", entry)
	}
	if err := h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.read(plainMDName); got != "alpha local\nbeta remote\n" {
		t.Fatalf("merged publication %q", got)
	}
	h.cloud.setFile(plainMDToken, []byte("alpha cloud second\nbeta remote\n"))
	entry = h.save(plainMDName, "alpha local second\nbeta remote\n")
	if err := h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	all, err := h.store.Conflicts(h.ctx)
	if err != nil || len(all) != 1 {
		t.Fatal(all, err)
	}
	c := all[0]
	if !strings.HasPrefix(c.RemoteRevision, "sha256:") {
		t.Fatal("missing content version", c)
	}
	host, err := ui.Start(h.ctx, ui.Options{Backend: conflictBackend{h}, RuntimeDir: filepath.Join(h.root, "plain-ui"), ConfigKey: "plain-md-http-integration", Notifier: noDesktop{}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	raw, err := host.BootstrapURL(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	csrf := ""
	request := func(method, path string, value any) (int, []byte) {
		t.Helper()
		body, _ := json.Marshal(value)
		req, err := http.NewRequest(method, host.URL()+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", host.URL())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		return r.StatusCode, b
	}
	status, data := request("POST", "/api/session", map[string]string{"bootstrap": u.Fragment[len("bootstrap="):]})
	if status != 200 {
		t.Fatal(status, string(data))
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	if err = json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	csrf = session.CSRF
	route := "/api/conflicts/" + string(c.ID)
	status, data = request("GET", route, nil)
	var detail ui.ConflictView
	if status != 200 || json.Unmarshal(data, &detail) != nil || !detail.Text || len(detail.Hunks) == 0 {
		t.Fatal("MD diff unavailable", status, string(data))
	}
	chosen := []byte("alpha human selected\nbeta remote\n")
	r := model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: "sha256:stale", Content: chosen}
	status, data = request("POST", route+"/resolve", r)
	if status != 409 {
		t.Fatal(status, string(data))
	}
	saved, _ := h.store.Conflicts(h.ctx)
	if len(saved) != 1 || h.blob(saved[0].Draft) != string(chosen) {
		t.Fatal("stale submit lost draft")
	}
	r.ExpectedRemoteRevision = c.RemoteRevision
	status, data = request("POST", route+"/resolve", r)
	if status != 200 {
		t.Fatal(status, string(data))
	}
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if h.read(plainMDName) != string(chosen) {
		t.Fatal("human resolution not published")
	}
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if h.cloud.uploads != 2 || h.cloud.applies != 0 || h.cloud.creates != 0 || string(h.cloud.resources[plainMDToken].data) != string(chosen) {
		t.Fatal("file/token contract changed", h.cloud.uploads, h.cloud.applies, h.cloud.creates)
	}
}
