package syncer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestUnchangedRemoteAcceptsEditedAndInsertedPlainParagraphs(t *testing.T) {
	s, engine, source, entry := setup(t, model.KindDocx)
	source.xml = `<title id="doc">Title</title><h1 id="heading">Heading</h1><p id="p">first</p>`
	source.revision++
	if err := engine.RefreshEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	local := []byte("# Heading\n\nsecond\n\nappended\n")
	if _, _, err := s.Stage(bg, entry.ID, nil, bytes.NewReader(local)); err != nil {
		t.Fatal(err)
	}
	if err := engine.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	view, err := markdown.Decode([]byte(source.xml), "9")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := markdown.Encode(local)
	got, _ := markdown.Encode(view.Markdown)
	if !bytes.Equal(want, got) {
		t.Fatalf("incomplete single-side sync: %s", source.xml)
	}
	current, err := s.Entry(bg, entry.ID)
	if err != nil || current.Status != model.StatusSynced {
		t.Fatalf("not synced: %+v %v", current, err)
	}
	if !strings.Contains(source.xml, "appended") {
		t.Fatal("inserted paragraph lost")
	}
}
