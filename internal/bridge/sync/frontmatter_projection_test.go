package syncer

import (
	"bytes"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestYAMLFrontMatterSnapshotProjectionIsExplicitAndFreshFetchPromotes(t *testing.T) {
	_, engine, source, entry := setup(t, model.KindDocx)
	xml, err := markdown.Encode([]byte("---\nname: synthetic\n---\n\nbody\n"))
	if err != nil {
		t.Fatal(err)
	}
	source.xml = string(xml)
	doc := cloud.Document{Token: entry.Token, Revision: "1", XML: xml}
	old, oldView, err := engine.saveDocProjection(bg, doc, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, loaded, err := engine.loadDoc(bg, old.ID)
	if err != nil || loaded.ProjectionVersion != 2 || !bytes.Equal(loaded.Markdown, oldView.Markdown) {
		t.Fatalf("old snapshot migrated: %+v %v", loaded, err)
	}
	fresh, view, err := engine.fetchDocProjection(bg, entry.Token, 2)
	if err != nil || view.ProjectionVersion != 3 || !bytes.HasPrefix(view.Markdown, []byte("---\n")) {
		t.Fatalf("fresh marker not promoted: %+v %v", view, err)
	}
	_, loaded, err = engine.loadDoc(bg, fresh.ID)
	if err != nil || loaded.ProjectionVersion != 3 || !bytes.Equal(loaded.Markdown, view.Markdown) {
		t.Fatalf("projection3 snapshot not loaded: %+v %v", loaded, err)
	}
	_, loaded, err = engine.loadDoc(bg, old.ID)
	if err != nil || loaded.ProjectionVersion != 2 || !bytes.Equal(loaded.Markdown, oldView.Markdown) {
		t.Fatal("fresh fetch changed old snapshot")
	}
}
