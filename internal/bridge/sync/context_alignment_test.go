package syncer

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestContextTableTimeoutReconcilesAndKeepsNewerSave(t *testing.T) {
	s, e, c, en := setup(t, model.KindDocx)
	first := `<table id="first"><thead><tr><th><p>State</p></th></tr></thead><tbody><tr><td><p>pending</p></td></tr></tbody></table>`
	second := strings.Replace(first, `id="first"`, `id="second"`, 1)
	c.xml = `<title id="doc">T</title><h1 id="a">Guangzhou</h1>` + first + `<h1 id="b">Yinchuan</h1>` + second + `<p id="tail">tail</p>`
	snapshot, _, err := e.saveDoc(bg, cloud.Document{Token: en.Token, Revision: "1", XML: []byte(c.xml)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.OfferRemote(bg, en.ID, en.Generation, snapshot, snapshot.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	en, err = s.CommitPublication(bg, candidate)
	if err != nil {
		t.Fatal(err)
	}
	body := blobText(t, s, en.Local)
	at := strings.LastIndex(body, "pending")
	if at < 0 {
		t.Fatal("fixture missing cell")
	}
	local := body[:at] + "deployed" + body[at+len("pending"):]
	en = stageContent(t, s, en, local)
	c.loseResponse = true
	var newer model.Entry
	c.applyHook = func() { newer = stageContent(t, s, en, strings.Replace(local, "tail", "newest local tail", 1)) }
	if err = e.SyncEntry(bg, en.ID); err != nil && !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal("older upload", err)
	}
	current, err := s.Entry(bg, en.ID)
	if err != nil || current.Local != newer.Local || !strings.Contains(blobText(t, s, current.Local), "newest local tail") {
		t.Fatal("newer input lost", err)
	}
	if c.writes != 1 || !strings.Contains(c.xml, first) || !strings.Contains(c.xml, "deployed") {
		t.Fatal("wrong table updated or unknown effect replayed", c.writes)
	}
	if err = e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal("latest generation", err)
	}
	current, err = s.Entry(bg, en.ID)
	if err != nil || current.Status != model.StatusSynced || !strings.Contains(c.xml, "newest local tail") || !strings.Contains(c.xml, first) {
		t.Fatal("latest completion did not preserve unrelated table", err)
	}
	writes := c.writes
	if err = e.SyncEntry(bg, en.ID); err != nil || c.writes != writes || c.creates != 0 || c.deletes != 0 {
		t.Fatal("completed operation replayed or document recreated", err, c.writes)
	}
}

func TestContextAutomaticSyncRejectsCloudSwapInvisibleInMarkdown(t *testing.T) {
	s, e, c, en := setup(t, model.KindDocx)
	table := `<table id="%s"><thead><tr><th><p>State</p></th></tr></thead><tbody><tr><td><p>pending</p></td></tr></tbody></table>`
	c.xml = `<title id="doc">T</title><h1 id="a">A</h1>` + fmt.Sprintf(table, "first") + `<h1 id="b">B</h1>` + fmt.Sprintf(table, "second")
	sn, _, err := e.saveDoc(bg, cloud.Document{Token: en.Token, Revision: "1", XML: []byte(c.xml)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.OfferRemote(bg, en.ID, en.Generation, sn, sn.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	en, err = s.CommitPublication(bg, candidate)
	if err != nil {
		t.Fatal(err)
	}
	body := blobText(t, s, en.Local)
	at := strings.LastIndex(body, "pending")
	local := body[:at] + "deployed" + body[at+len("pending"):]
	en = stageContent(t, s, en, local)
	c.xml = strings.ReplaceAll(c.xml, `id="first"`, `id="temporary"`)
	c.xml = strings.ReplaceAll(c.xml, `id="second"`, `id="first"`)
	c.xml = strings.ReplaceAll(c.xml, `id="temporary"`, `id="second"`)
	c.revision++
	_ = e.SyncEntry(bg, en.ID)
	if c.writes != 0 {
		t.Fatal("normal automatic path wrote the other original table", c.writes)
	}
	current, err := s.Entry(bg, en.ID)
	if err != nil || blobText(t, s, current.Local) != local {
		t.Fatal("blocked local edit lost", err)
	}
}
