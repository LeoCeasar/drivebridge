package overlay

import (
	"context"
	"errors"
	"io"
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
	waitForDocumentMedia(t, f, en)
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
	en := acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	file, err := managed.Open("/manual.md")
	if err != nil {
		t.Fatal("native absolute path rejected", err)
	}
	file.Close()
	waitForDocumentMedia(t, f, en)
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

func TestCachedDocumentReadSurvivesMissingImagesOffline(t *testing.T) {
	f, st, base := fixture(t)
	f.source = &assetSource{Source: base, err: errors.New("offline")}
	acceptAssets(t, st, "manual.md", "doc", "accepted text", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	if err := f.PrepareManagedRead(ctx, "manual.md"); err != nil {
		t.Fatal("image failure rejected locally accepted text", err)
	}
	obj, err := f.NewObject(ctx, "manual.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := body(t, obj); got != "accepted text" {
		t.Fatal(got)
	}
}

type stalledMediaSource struct {
	Source
	entered chan struct{}
	release chan struct{}
}

func (s *stalledMediaSource) OpenDocMedia(ctx context.Context, token string) (io.ReadCloser, error) {
	if token != "image" {
		return io.NopCloser(strings.NewReader("other image")), nil
	}
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.release:
		return nil, errors.New("offline")
	}
}

func TestCachedDocumentReadsDoNotWaitForImageCloud(t *testing.T) {
	f, st, base := fixture(t)
	source := &stalledMediaSource{Source: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.source = source
	defer close(source.release)
	acceptAssets(t, st, "manual.md", "doc", "accepted text", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	result := make(chan error, 1)
	go func() { result <- f.PrepareManagedRead(ctx, "manual.md") }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cached text open waited for image download")
	}
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("legacy media was not scheduled for backfill")
	}
	for i := 0; i < 10; i++ {
		if err := f.PrepareManagedRead(ctx, "manual.md"); err != nil {
			t.Fatal(err)
		}
	}
	f.docMediaMu.Lock()
	queued := len(f.docMediaQueue)
	f.docMediaMu.Unlock()
	if queued != 0 {
		t.Fatal("repeated cached reads queued duplicate downloads", queued)
	}
	obj, err := f.NewObject(ctx, "manual.md")
	if err != nil || body(t, obj) != "accepted text" {
		t.Fatal("stalled media hid cached text", err)
	}
	// An explicit image open for a different document is independent of the
	// blocked background download.
	acceptAssets(t, st, "other.md", "other", "1", []model.DocAsset{{Token: "other-image", Path: assetPath("other-image")}})
	other, err := f.NewObject(ctx, assetPath("other-image"))
	if err != nil {
		t.Fatal(err)
	}
	imageResult := make(chan error, 1)
	go func() {
		r, err := other.Open(ctx)
		if err == nil {
			_, err = io.Copy(io.Discard, r)
			r.Close()
		}
		imageResult <- err
	}()
	select {
	case err := <-imageResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated image open waited for blocked document")
	}
	if f.Features().Shutdown == nil {
		t.Fatal("background media worker has no shutdown")
	}
	if err := f.Features().Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentAssetManifestReusesVerifiedImmutableSnapshot(t *testing.T) {
	f, st, _ := fixture(t)
	p := assetPath("image")
	en := acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
	if _, err := f.NewObject(ctx, p); err != nil {
		t.Fatal(err)
	}
	sn := mustSnapshot(t, st, en.RemoteCheckpoint)
	r, err := st.ReadBlob(ctx, sn.XML)
	if err != nil {
		t.Fatal(err)
	}
	name := r.(interface{ Name() string }).Name()
	r.Close()
	if err := os.Rename(name, name+".held"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(name+".held", name)
	for i := 0; i < 3; i++ {
		if _, err := f.NewObject(ctx, p); err != nil {
			t.Fatal("warm asset metadata reparsed original XML", err)
		}
		if _, err := f.List(ctx, ""); err != nil {
			t.Fatal("warm directory metadata reparsed original XML", err)
		}
	}
}

func TestCompleteImageCacheEnsureDoesNotReparseXML(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "cached image"}
	f.source = source
	en := acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	if err := f.ensureDocImages(ctx, en); err != nil {
		t.Fatal(err)
	}
	sn := mustSnapshot(t, st, en.RemoteCheckpoint)
	r, err := st.ReadBlob(ctx, sn.XML)
	if err != nil {
		t.Fatal(err)
	}
	name := r.(interface{ Name() string }).Name()
	r.Close()
	if err := os.Rename(name, name+".held"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(name+".held", name)
	for i := 0; i < 3; i++ {
		if err := doccache.Ensure(ctx, st, source, sn); err != nil {
			t.Fatal("complete cached images require original XML", err)
		}
	}
	if source.opens != 1 {
		t.Fatal("complete cached images downloaded again", source.opens)
	}
}

func waitForDocumentMedia(t *testing.T, f *Fs, en model.Entry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.docMediaMu.Lock()
		ready := f.docMediaStates[en.RemoteCheckpoint].ready
		f.docMediaMu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background document media backfill did not complete")
}

func TestLegacyImageBackfillRetriesAfterFailureWithoutBlockingText(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, err: errors.New("offline")}
	f.source = source
	en := acceptAssets(t, st, "manual.md", "doc", "accepted text", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	if err := f.PrepareManagedRead(ctx, en.Remote); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	failed := false
	for time.Now().Before(deadline) {
		f.docMediaMu.Lock()
		state := f.docMediaStates[en.RemoteCheckpoint]
		failed = !state.queued && !state.retryAt.IsZero()
		f.docMediaMu.Unlock()
		if failed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !failed {
		t.Fatal("failed backfill did not become retryable")
	}
	for i := 0; i < 3; i++ {
		if err := f.PrepareManagedRead(ctx, en.Remote); err != nil {
			t.Fatal(err)
		}
	}
	if source.opens != 1 {
		t.Fatal("offline text reads retried media without backoff", source.opens)
	}
	source.err = nil
	source.body = "recovered image"
	// Advance just this snapshot past its retry cooldown without a wall-clock
	// minute of sleeping. The subsequent read still only schedules the worker.
	f.docMediaMu.Lock()
	state := f.docMediaStates[en.RemoteCheckpoint]
	state.retryAt = time.Time{}
	f.docMediaStates[en.RemoteCheckpoint] = state
	f.docMediaMu.Unlock()
	if err := f.PrepareManagedRead(ctx, en.Remote); err != nil {
		t.Fatal(err)
	}
	waitForDocumentMedia(t, f, en)
	if source.opens != 2 {
		t.Fatal("legacy image did not recover in background", source.opens)
	}
	for i := 0; i < 3; i++ {
		if err := f.PrepareManagedRead(ctx, en.Remote); err != nil {
			t.Fatal(err)
		}
	}
	if source.opens != 2 {
		t.Fatal("ready cache downloaded again", source.opens)
	}
}
