package syncer

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestScheme3SnapshotPersistsProjectionAndAssets(t *testing.T) {
	s, e, _, _ := setup(t, model.KindDocx)
	sn, view, err := e.saveDoc(bg, cloud.Document{Token: "image-doc", Revision: "1", XML: []byte(`<title id="doc">Image</title><img id="image" src="image-token" mime="image/png" name="test.png"/>`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.read(bg, sn.BlockMap)
	if err != nil {
		t.Fatal(err)
	}
	var side model.DocSidecar
	if err = json.Unmarshal(raw, &side); err != nil {
		t.Fatal(err)
	}
	if side.ProjectionVersion != 2 || len(side.Assets) != 1 || side.Assets[0].Token != "image-token" {
		t.Fatalf("projection context not persisted: %+v", side)
	}
	_, loaded, err := e.loadDoc(bg, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProjectionVersion != 2 || !bytes.Equal(loaded.Markdown, view.Markdown) || len(loaded.Assets) != 1 {
		t.Fatal("snapshot did not round trip its projection")
	}
	if err = s.Validate(bg); err != nil {
		t.Fatal(err)
	}
}

func legacyProjectionFixture(t *testing.T) (*store.Store, *Engine, *fixtureCloud, model.Entry) {
	t.Helper()
	s, e, source, entry := setup(t, model.KindDocx)
	source.xml = `<title id="doc">Title</title><p id="a">before</p><p id="rich" color="red">preserve rich content</p>`
	sn, _, err := e.saveDocProjection(bg, cloud.Document{Token: entry.Token, Revision: "1", XML: []byte(source.xml)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.OfferRemote(bg, entry.ID, entry.Generation, sn, sn.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	entry, err = s.CommitPublication(bg, c)
	if err != nil {
		t.Fatal(err)
	}
	return s, e, source, entry
}

func TestScheme3CleanLegacyUpgradeWithoutRemoteRevisionChange(t *testing.T) {
	s, e, _, entry := legacyProjectionFixture(t)
	if err := e.RefreshEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Entry(bg, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, view, err := e.loadDoc(bg, got.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if view.ProjectionVersion != 2 || !strings.Contains(string(view.Markdown), "preserve rich content") {
		t.Fatalf("clean cache not upgraded: version %d %q", view.ProjectionVersion, view.Markdown)
	}
}

func TestScheme3LegacyQueuedSaveRetainsProjectionVersion(t *testing.T) {
	s, e, source, entry := legacyProjectionFixture(t)
	base := blobText(t, s, entry.Local)
	entry = stageContent(t, s, entry, strings.Replace(base, "before", "after", 1))
	if err := e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Entry(bg, entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, view, err := e.loadDoc(bg, got.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusSynced || view.ProjectionVersion != 1 || source.writes != 1 || !strings.Contains(source.xml, `<p id="rich" color="red">preserve rich content</p>`) {
		t.Fatalf("legacy edit changed interpretation: status=%s version=%d writes=%d", got.Status, view.ProjectionVersion, source.writes)
	}
}

func TestScheme3LegacyConflictReviewRetainsProjectionVersion(t *testing.T) {
	s, e, source, entry := legacyProjectionFixture(t)
	base := blobText(t, s, entry.Local)
	stageContent(t, s, entry, strings.Replace(base, "before", "local edit", 1))
	source.xml = strings.Replace(source.xml, ">before</p>", ">remote edit</p>", 1)
	source.revision++
	if err := e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatalf("expected conflict: %v", err)
	}
	conflicts, err := s.Conflicts(bg)
	if err != nil || len(conflicts) != 1 {
		t.Fatalf("conflicts %d %v", len(conflicts), err)
	}
	fresh, err := e.RefreshConflict(bg, conflicts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	remote := blobText(t, s, fresh.Remote)
	if !strings.Contains(remote, "Unsupported p content") {
		t.Fatal("review silently reprojected pending legacy conflict")
	}
	_, job, err := e.Resolve(bg, model.ResolveRequest{ConflictID: fresh.ID, ExpectedGeneration: fresh.Generation, ExpectedRemoteRevision: fresh.RemoteRevision, Content: []byte(strings.Replace(remote, "remote edit", "resolved edit", 1))})
	if err != nil {
		t.Fatal(err)
	}
	_, view, err := e.loadDoc(bg, job.BaseRemote)
	if err != nil {
		t.Fatal(err)
	}
	if view.ProjectionVersion != 1 || source.writes != 0 {
		t.Fatalf("resolution changed interpretation or wrote cloud: version=%d writes=%d", view.ProjectionVersion, source.writes)
	}
}

func TestScheme3LegacySidecarUsesOriginalProjection(t *testing.T) {
	s, e, _, _ := setup(t, model.KindDocx)
	x, err := s.PutBlob(bg, strings.NewReader(`<p id="p" color="red">legacy text</p>`))
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.PutBlob(bg, strings.NewReader("Unsupported p content (online source unavailable)\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.PutBlob(bg, strings.NewReader(`{"blocks":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	sn, err := s.SaveSnapshot(bg, model.Snapshot{Token: "legacy", Revision: "1", XML: x, Markdown: m, BlockMap: b, Size: 56})
	if err != nil {
		t.Fatal(err)
	}
	_, view, err := e.loadDoc(bg, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.ProjectionVersion != 1 || string(view.Markdown) != "Unsupported p content (online source unavailable)\n" {
		t.Fatalf("legacy baseline silently reprojected: version %d %q", view.ProjectionVersion, view.Markdown)
	}
}

func TestScheme3RejectsUnknownProjectionAndCorruptBaseline(t *testing.T) {
	for _, tc := range []struct{ name, side, md string }{{"unknown", `{"projection_version":99}`, "body\n"}, {"different-body", `{"projection_version":1}`, "other body\n"}} {
		t.Run(tc.name, func(t *testing.T) {
			s, e, _, _ := setup(t, model.KindDocx)
			x, err := s.PutBlob(bg, strings.NewReader(`<p id="p">body</p>`))
			if err != nil {
				t.Fatal(err)
			}
			m, err := s.PutBlob(bg, strings.NewReader(tc.md))
			if err != nil {
				t.Fatal(err)
			}
			b, err := s.PutBlob(bg, strings.NewReader(tc.side))
			if err != nil {
				t.Fatal(err)
			}
			sn, err := s.SaveSnapshot(bg, model.Snapshot{Token: "legacy", Revision: "1", XML: x, Markdown: m, BlockMap: b, Size: int64(len(tc.md))})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = e.loadDoc(bg, sn.ID)
			if !errors.Is(err, model.ErrUnsupportedContent) {
				t.Fatalf("unusable baseline accepted: %v", err)
			}
		})
	}
}
