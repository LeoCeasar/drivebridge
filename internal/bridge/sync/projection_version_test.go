package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type imageFixtureCloud struct {
	*fixtureCloud
	imageErr   error
	imageReads int
}

func (s *imageFixtureCloud) OpenDocMedia(context.Context, string) (io.ReadCloser, error) {
	s.imageReads++
	if s.imageErr != nil {
		return nil, s.imageErr
	}
	return io.NopCloser(strings.NewReader("image data")), nil
}

func TestScheme3SnapshotPersistsProjectionAndAssets(t *testing.T) {
	s, e, source, _ := setup(t, model.KindDocx)
	e.source = &imageFixtureCloud{fixtureCloud: source}
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

func TestRefreshCachesImagesEvenWhenRevisionAndMarkdownUnchanged(t *testing.T) {
	s, e, source, en := setup(t, model.KindDocx)
	xml := []byte(`<p id="p">hello</p><img id="i" src="image"/>`)
	source.xml = string(xml)
	imageSource := &imageFixtureCloud{fixtureCloud: source}
	e.source = imageSource
	// Simulate an existing v2 snapshot written before bundled image caching.
	sn, _, err := e.saveDoc(bg, cloud.Document{Token: en.Token, Revision: "1", XML: xml})
	if err != nil {
		t.Fatal(err)
	}
	sn.ID = ""
	sn.DocMedia = nil
	sn, err = s.SaveSnapshot(bg, sn)
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
	if err = e.RefreshEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Entry(bg, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := s.Snapshot(bg, got.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached.DocMedia) != 1 {
		t.Fatal("unchanged document did not acquire durable images")
	}
	reads := imageSource.imageReads
	if err = e.RefreshEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	if imageSource.imageReads != reads {
		t.Fatal("same revision re-downloaded images")
	}
}

func TestImageNotFoundDoesNotDeleteDocument(t *testing.T) {
	s, e, source, en := setup(t, model.KindDocx)
	source.xml = `<p id="p">hello</p><img id="i" src="image"/>`
	source.items = nil // An image error must not initiate document-deletion discovery.
	e.source = &imageFixtureCloud{fixtureCloud: source, imageErr: store.ErrNotFound}
	if err := e.RefreshEntry(bg, en.ID); err == nil {
		t.Fatal("missing image was ignored")
	}
	m, err := s.Metadata(bg, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tombstone {
		t.Fatal("image failure deleted the document")
	}
}

func TestBackgroundBackfillCachesExistingImagesWithoutChangingEntry(t *testing.T) {
	s, e, source, en := setup(t, model.KindDocx)
	xml := []byte(`<p id="p">hello</p><img id="i" src="image"/>`)
	imageSource := &imageFixtureCloud{fixtureCloud: source}
	e.source = imageSource
	view, err := markdown.Decode(xml, "1")
	if err != nil {
		t.Fatal(err)
	}
	blob := func(b []byte) model.BlobID {
		id, err := s.PutBlob(bg, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	side, err := json.Marshal(model.DocSidecar{ProjectionVersion: view.ProjectionVersion, Assets: view.Assets, Blocks: view.Blocks})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := s.SaveSnapshot(bg, model.Snapshot{Token: en.Token, Revision: "1", XML: blob(xml), Markdown: blob(view.Markdown), BlockMap: blob(side), Size: int64(len(view.Markdown))})
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
	en = stageContent(t, s, en, "unsent local change")
	if _, err = s.ObserveEntry(bg, store.Observation{Remote: "cold.md", Token: "cold", Kind: model.KindDocx}); err != nil {
		t.Fatal(err)
	}
	source.readErr = errors.New("no document network request allowed")
	e.backfillCachedDocImages(bg)
	after, err := s.Entry(bg, en.ID)
	if err != nil || after != en {
		t.Fatal("backfill changed pending draft", err)
	}
	cached, err := s.CachedDocMedia(bg, en.Token, "1")
	if err != nil || len(cached) != 1 {
		t.Fatal("background did not cache image", err)
	}
	if imageSource.imageReads != 1 {
		t.Fatal("expected exactly one image download", imageSource.imageReads)
	}
	e.backfillCachedDocImages(bg)
	if imageSource.imageReads != 1 {
		t.Fatal("second pass re-downloaded image")
	}
}
