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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ui"
)

// This test adapter exercises the real HTTP/Store/Engine resolution contract.
// Production manager IPC is intentionally not bypassed by the production UI.
type conflictBackend struct{ h *harness }

func (conflictBackend) Status(context.Context) (ui.StatusView, error) {
	return ui.StatusView{}, model.ErrUnsupportedContent
}
func (conflictBackend) Config(context.Context) (ui.ConfigView, error) {
	return ui.ConfigView{}, model.ErrUnsupportedContent
}
func (conflictBackend) Configure(context.Context, ui.ConfigRequest) (ui.ConfigView, error) {
	return ui.ConfigView{}, model.ErrUnsupportedContent
}
func (conflictBackend) AuthStart(context.Context, model.ProfileID) (auth.AuthFlow, error) {
	return auth.AuthFlow{}, model.ErrUnsupportedContent
}
func (conflictBackend) AuthComplete(context.Context, string) error {
	return model.ErrUnsupportedContent
}
func (conflictBackend) Mount(context.Context, model.MountID, string) error {
	return model.ErrUnsupportedContent
}
func (b conflictBackend) Conflict(ctx context.Context, id model.ConflictID) (ui.ConflictInputs, error) {
	all, err := b.h.store.Conflicts(ctx)
	if err != nil {
		return ui.ConflictInputs{}, err
	}
	for _, c := range all {
		if c.ID != id {
			continue
		}
		e, err := b.h.store.Entry(ctx, c.EntryID)
		if err != nil {
			return ui.ConflictInputs{}, err
		}
		read := func(id model.BlobID) ([]byte, error) {
			if id == "" {
				return nil, nil
			}
			r, err := b.h.store.ReadBlob(ctx, id)
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}
		v := ui.ConflictInputs{Conflict: c, Entry: e, Text: e.Kind == model.KindDocx || model.IsMarkdownFile(e)}
		if v.Base, err = read(c.Base); err != nil {
			return v, err
		}
		if v.Local, err = read(c.Local); err != nil {
			return v, err
		}
		if v.Remote, err = read(c.Remote); err != nil {
			return v, err
		}
		v.Draft, err = read(c.Draft)
		return v, err
	}
	return ui.ConflictInputs{}, os.ErrNotExist
}
func (b conflictBackend) SaveDraft(ctx context.Context, id model.ConflictID, content []byte) error {
	return b.h.store.SaveDraft(ctx, id, bytes.NewReader(content))
}
func (b conflictBackend) Resolve(ctx context.Context, r model.ResolveRequest) error {
	_, _, err := b.h.engine.Resolve(ctx, r)
	return err
}

type noDesktop struct{}

func (noDesktop) Available() bool { return false }
func (noDesktop) Notify(context.Context, string, string) error {
	return errors.New("unexpected native notification in integration test")
}
func (noDesktop) Open(context.Context, string) error {
	return errors.New("unexpected browser in integration test")
}
func TestConflictHTTP409RetainsDurableDraftAndResolvedBytesReachVFS(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	h.cloud.setDoc(h.docToken, `<p id="a">alpha REMOTE gamma</p><p id="anchor">stable anchor 中文😀</p><p id="b">tail</p>`)
	entry := h.save("document.md", "alpha LOCAL gamma\n\nstable anchor 中文😀\n\ntail\n")
	if err := h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	all, err := h.store.Conflicts(h.ctx)
	if err != nil || len(all) != 1 {
		t.Fatal(all, err)
	}
	c := all[0]
	host, err := ui.Start(h.ctx, ui.Options{Backend: conflictBackend{h}, RuntimeDir: filepath.Join(h.root, "ui-runtime"), ConfigKey: "synthetic-integration", Notifier: noDesktop{}})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close(context.Background())
	raw, err := host.BootstrapURL(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, _ := url.Parse(raw)
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
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, data
	}
	status, data := request("POST", "/api/session", map[string]string{"bootstrap": strings.TrimPrefix(bootstrap.Fragment, "bootstrap=")})
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
	chosen := []byte("alpha human chosen gamma\n\nstable anchor 中文😀\n\ntail\n")
	r := model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: "stale", Content: chosen}
	route := "/api/conflicts/" + string(c.ID) + "/resolve"
	status, data = request("POST", route, r)
	if status != 409 {
		t.Fatal(status, string(data))
	}
	after, err := h.store.Conflicts(h.ctx)
	if err != nil || len(after) != 1 || h.blob(after[0].Draft) != string(chosen) {
		t.Fatal(after, err)
	}
	r.ExpectedRemoteRevision = c.RemoteRevision
	status, data = request("POST", route, r)
	if status != 200 {
		t.Fatal(status, string(data))
	}
	if strings.Contains(h.cloud.doc(t, h.docToken), "human chosen") {
		t.Fatal("HTTP resolve uploaded before queued sync")
	}
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	if h.read("document.md") != string(chosen) {
		t.Fatal("HTTP chosen bytes were not published through VFS")
	}
}
