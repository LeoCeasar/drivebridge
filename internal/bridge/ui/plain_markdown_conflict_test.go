package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestOrdinaryMarkdownHashConflictUsesExistingDiffAndResolutionAPI(t *testing.T) {
	h, b, c, csrf, _ := fixture(t)
	b.mu.Lock()
	b.input.Entry.Kind = model.KindFile
	b.input.Entry.Remote = "plain-source.MD"
	b.input.Conflict.RemoteRevision = "sha256:" + strings.Repeat("a", 64)
	id := b.input.Conflict.ID
	b.mu.Unlock()
	res := request(t, c, h, "GET", "/api/conflicts/"+string(id), nil, "", "")
	var view ConflictView
	err := json.NewDecoder(res.Body).Decode(&view)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || !view.Text || len(view.Hunks) == 0 {
		t.Fatal(view, res.Status, err)
	}
	r := model.ResolveRequest{ConflictID: id, ExpectedGeneration: 4, ExpectedRemoteRevision: "sha256:" + strings.Repeat("a", 64), Content: []byte("resolved plain Markdown\r\n")}
	res = request(t, c, h, "POST", "/api/conflicts/"+string(id)+"/resolve", r, csrf, h.URL())
	res.Body.Close()
	if res.StatusCode != http.StatusOK || b.resolves != 1 {
		t.Fatal(res.Status, b.resolves)
	}
	saved, _ := os.ReadFile(b.draftFile)
	if string(saved) != string(r.Content) {
		t.Fatal(string(saved))
	}
}
