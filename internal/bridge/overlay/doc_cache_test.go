package overlay

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/doccache"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestDocumentReadCachesImagesBeforePublishing(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "cached image bytes"}
	f.source = source
	base.docs["doc"] = `<p id="p">hello</p><img id="a" src="image"/><img id="b" src="second"/>`
	en, err := st.ObserveEntry(ctx, store.Observation{Remote: "manual.md", Token: "doc", Kind: model.KindDocx})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.NewObject(ctx, en.Remote)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body(t, obj), "hello") {
		t.Fatal("missing document")
	}
	if source.opens != 2 {
		t.Fatalf("document published without caching both images: downloads=%d", source.opens)
	}
	source.err = errors.New("offline")
	for _, token := range []string{"image", "second"} {
		image, err := f.NewObject(ctx, assetPath(token))
		if err != nil {
			t.Fatal(err)
		}
		if image.Size() != int64(len(source.body)) {
			t.Fatalf("cached image has unknown size: %d", image.Size())
		}
		if got := body(t, image); got != source.body {
			t.Fatal(got)
		}
	}
	if source.opens != 2 {
		t.Fatal("image display made a network request")
	}
}

func TestDocumentImageFailureDoesNotPublishIncompleteCache(t *testing.T) {
	f, st, base := fixture(t)
	f.source = &assetSource{Source: base, err: errors.New("offline")}
	base.docs["doc"] = `<p id="p">hello</p><img id="a" src="image"/>`
	en, err := st.ObserveEntry(ctx, store.Observation{Remote: "manual.md", Token: "doc", Kind: model.KindDocx})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.NewObject(ctx, en.Remote)
	if err != nil {
		t.Fatal(err)
	}
	r, err := obj.Open(ctx)
	if err == nil {
		r.Close()
		t.Fatal("incomplete document published despite image failure")
	}
	got, err := st.Entry(ctx, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Visible != "" || got.RemoteCheckpoint != "" {
		t.Fatal("failed download changed accepted document")
	}
}

func TestLegacyImageBackfillPreservesPendingDraft(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "cached image"}
	f.source = source
	en := acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	staged, _, err := st.Stage(ctx, en.ID, &en.Generation, strings.NewReader("my unsent draft"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := st.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.PrepareManagedRead(ctx, "manual.md"); err != nil {
		t.Fatal(err)
	}
	after, err := st.Entry(ctx, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != staged {
		t.Fatal("backfill changed draft, baseline or generation")
	}
	jobs, err := st.Pending(ctx)
	if err != nil || len(jobs) != len(before) || jobs[0].ID != before[0].ID {
		t.Fatal("backfill changed pending job")
	}
	source.err = errors.New("offline")
	image, err := f.NewObject(ctx, assetPath("image"))
	if err != nil {
		t.Fatal(err)
	}
	if got := body(t, image); got != source.body {
		t.Fatal(got)
	}
	if source.opens != 1 {
		t.Fatal("backfill downloaded more than once", source.opens)
	}
}

func TestDocumentImagesFollowSnapshotGCAndCleanEviction(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "durable image"}
	f.source = source
	base.docs["doc"] = `<p id="p">hello</p><img id="a" src="image"/>`
	en, err := st.ObserveEntry(ctx, store.Observation{Remote: "manual.md", Token: "doc", Kind: model.KindDocx})
	if err != nil {
		t.Fatal(err)
	}
	obj, err := f.NewObject(ctx, en.Remote)
	if err != nil {
		t.Fatal(err)
	}
	body(t, obj)
	en, err = st.Entry(ctx, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	sn, err := st.Snapshot(ctx, en.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	image := sn.DocMedia[assetPath("image")]
	if image == "" {
		t.Fatal("no durable image reference")
	}
	r, err := st.ReadBlob(ctx, image)
	if err != nil {
		t.Fatal(err)
	}
	fileName := r.(interface{ Name() string }).Name()
	r.Close()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(fileName, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GC(ctx, store.GCOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err = st.ReadBlob(ctx, image)
	if err != nil {
		t.Fatal("GC removed accepted document image", err)
	}
	archiveStoreHistory(t, st)
	// An image read lease protects the document's whole cache bundle.
	if evicted, err := st.EvictCleanEntry(ctx, en.ID, en.Generation); err != nil || evicted {
		t.Fatal("evicted open image", evicted, err)
	}
	r.Close()
	if evicted, err := st.EvictCleanEntry(ctx, en.ID, en.Generation); err != nil || !evicted {
		t.Fatal("closed clean cache not evicted", evicted, err)
	}
	if r, err := st.ReadBlob(ctx, image); err == nil {
		r.Close()
		t.Fatal("image body survived clean bundle eviction")
	}
}

func TestBackfilledImagesSurviveGCOfUnreferencedSnapshotHistory(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "legacy image"}
	f.source = source
	en := acceptAssets(t, st, "manual.md", "doc", "old", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	if err := f.ensureDocImages(ctx, en); err != nil {
		t.Fatal(err)
	}
	cached, err := st.CachedDocMedia(ctx, "doc", "old")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		sn, err := st.Snapshot(ctx, en.RemoteCheckpoint)
		if err != nil {
			t.Fatal(err)
		}
		sn.ID = ""
		sn.Revision = "new"
		if _, err = st.SaveSnapshot(ctx, sn); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.GC(ctx, store.GCOptions{Now: time.Now().Add(60 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	source.err = errors.New("offline")
	if err := doccache.Ensure(ctx, st, source, mustSnapshot(t, st, en.RemoteCheckpoint)); err != nil {
		t.Fatal("GC lost images of referenced legacy baseline", err)
	}
	if len(cached) != 1 {
		t.Fatal("missing cache")
	}
}
func mustSnapshot(t *testing.T, st *store.Store, id model.SnapshotID) model.Snapshot {
	t.Helper()
	sn, err := st.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return sn
}

func TestDocumentImageBackfillThroughAbsoluteNativeOpen(t *testing.T) {
	f, st, base, managed, _ := bodyCacheIntegration(t, nil)
	source := &assetSource{Source: base, body: "cached native image"}
	f.source = source
	acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	file, err := managed.Open("/manual.md")
	if err != nil {
		t.Fatal("native absolute path rejected", err)
	}
	file.Close()
	if source.opens != 1 {
		t.Fatal("native document open failed to prepare images", source.opens)
	}
}

func TestLegacyImagesRemainWhenAnotherEntryStillReferencesDocument(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "shared legacy image"}
	f.source = source
	assets := []model.DocAsset{{Token: "image", Path: assetPath("image")}}
	first := acceptAssets(t, st, "first.md", "doc", "1", assets)
	acceptAssets(t, st, "second.md", "doc", "1", assets)
	if err := f.ensureDocImages(ctx, first); err != nil {
		t.Fatal(err)
	}
	cached, err := st.CachedDocMedia(ctx, "doc", "1")
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.ReadBlob(ctx, cached[assetPath("image")])
	if err != nil {
		t.Fatal(err)
	}
	name := r.(interface{ Name() string }).Name()
	r.Close()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.EvictCleanEntry(ctx, first.ID, first.Generation); err != nil || !ok {
		t.Fatal(ok, err)
	}
	source.err = errors.New("offline")
	image, err := f.NewObject(ctx, assetPath("image"))
	if err != nil {
		t.Fatal(err)
	}
	if body(t, image) != source.body {
		t.Fatal("other entry lost shared image cache")
	}
}
