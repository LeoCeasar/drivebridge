package overlay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

type assetSource struct {
	Source
	opens int
	body  string
	err   error
}

func (s *assetSource) OpenDocMedia(context.Context, string) (io.ReadCloser, error) {
	s.opens++
	if s.err != nil {
		return nil, s.err
	}
	return &assetReader{Reader: bytes.NewReader([]byte(s.body))}, nil
}

type assetReader struct{ *bytes.Reader }

func (*assetReader) Close() error { return nil }
func assetPath(token string) string {
	return fmt.Sprintf(".drivebridge-assets/%x.png", sha256.Sum256([]byte(token)))
}
func acceptAssets(t *testing.T, st *store.Store, remote, token, revision string, assets []model.DocAsset) model.Entry {
	t.Helper()
	e, err := st.ObserveEntry(ctx, store.Observation{Remote: remote, Kind: model.KindDocx, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := json.Marshal(model.DocSidecar{ProjectionVersion: 2, Assets: assets})
	if err != nil {
		t.Fatal(err)
	}
	blob := func(b []byte) model.BlobID {
		id, e := st.PutBlob(ctx, bytes.NewReader(b))
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	var xml strings.Builder
	xml.WriteString(`<p id="p">` + html.EscapeString(revision) + `</p>`)
	for i, a := range assets {
		fmt.Fprintf(&xml, `<img id="i%d" src="%s" name="%s" mime="%s"/>`, i, html.EscapeString(a.Token), html.EscapeString(a.Name), html.EscapeString(a.MIME))
	}
	sn := model.Snapshot{Token: token, Revision: revision, XML: blob([]byte(xml.String())), Markdown: blob([]byte(revision)), BlockMap: blob(sidecar)}
	candidate, err := st.OfferRemote(ctx, e.ID, e.Generation, sn, sn.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	e, err = st.CommitPublication(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestDocAssetsDurableLazyAndRanges(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "0123456789"}
	f.source = source
	p := assetPath("image")
	acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p, MIME: "image/png"}})
	entries, err := f.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Remote() == ".drivebridge-assets" {
			found = true
		}
	}
	if !found {
		t.Fatal("virtual directory not listed")
	}
	entries, err = f.List(ctx, ".drivebridge-assets")
	if err != nil || len(entries) != 1 || entries[0].Remote() != p || entries[0].Size() != -1 {
		t.Fatal(entries, err)
	}
	o, err := f.NewObject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if source.opens != 0 || base.fetches != 0 || o.Size() != -1 {
		t.Fatal("metadata fetched images", source.opens, o.Size())
	}
	if got := body(t, o, &fs.RangeOption{Start: 2, End: 5}); got != "2345" {
		t.Fatal(got)
	}
	if o.Size() != 10 {
		t.Fatal(o.Size())
	}
	if got := body(t, o, &fs.RangeOption{Start: -1, End: 3}); got != "789" {
		t.Fatal(got)
	}
	if got := body(t, o, &fs.SeekOption{Offset: 8}); got != "89" {
		t.Fatal(got)
	}
	sc, _ := st.Scope(ctx)
	raw, err := New(Dependencies{Store: st, Source: source, ProfileID: sc.ProfileID, MountID: sc.MountID})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := raw.NewObject(ctx, p)
	if err != nil || body(t, reopened) != "0123456789" {
		t.Fatal(reopened, err)
	}
	if _, err = st.EntryByRemote(ctx, p); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("virtual asset polluted store", err)
	}
	jobs, err := st.Pending(ctx)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}
func TestDocAssetsReadonlyAndScoped(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, body: "image"}
	f.source = source
	if err := f.Mkdir(ctx, "folder"); err != nil {
		t.Fatal(err)
	}
	p := assetPath("image")
	acceptAssets(t, st, "folder/manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
	remote := "folder/" + p
	o, err := f.NewObject(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	info := object.NewStaticObjectInfo(remote, time.Now(), 3, true, nil, f)
	rejects := []func() error{
		func() error { return o.Update(ctx, strings.NewReader("bad"), info) }, func() error { return o.Remove(ctx) }, func() error { return o.SetModTime(ctx, time.Now()) },
		func() error { _, e := f.Put(ctx, strings.NewReader("bad"), info); return e }, func() error { return f.Mkdir(ctx, "folder/.drivebridge-assets") }, func() error { return f.Rmdir(ctx, "folder/.drivebridge-assets") },
		func() error { _, e := f.BeginManagedRename(ctx, remote, "copy.png"); return e }, func() error { _, e := f.BeginManagedRename(ctx, "new.png", remote); return e },
	}
	for i, reject := range rejects {
		if err := reject(); !errors.Is(err, fs.ErrorPermissionDenied) {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	for _, bad := range []string{p, "folder/.drivebridge-assets/forged.png", "folder/.drivebridge-assets/../manual.md", "folder/.drivebridge-assets/extra/" + strings.TrimPrefix(p, ".drivebridge-assets/")} {
		if _, err := f.NewObject(ctx, bad); err == nil {
			t.Fatal("accepted forged/cross-directory path", bad)
		}
	}
	if source.opens != 0 {
		t.Fatal("rejected mutation fetched image")
	}
	if o.Storable() {
		t.Fatal("asset storable")
	}
	acceptAssets(t, st, "folder/manual.md", "doc", "2", nil)
	if _, err := o.Open(ctx); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatal("stale handle retained authority", err)
	}
}
func TestDocAssetsRejectForgedSidecarAndFailure(t *testing.T) {
	f, st, base := fixture(t)
	source := &assetSource{Source: base, err: errors.New("download failed")}
	f.source = source
	p := assetPath("image")
	acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "other", Path: p}, {Token: "image", Path: "../secret"}})
	if _, err := f.NewObject(ctx, p); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatal(err)
	}
	acceptAssets(t, st, "manual.md", "doc", "2", []model.DocAsset{{Token: "image", Path: p}})
	o, err := f.NewObject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := o.Open(ctx); err == nil || r != nil {
		t.Fatal("failed media became empty content", r, err)
	}
	if o.Size() != -1 {
		t.Fatal(o.Size())
	}
}

func TestDocAssetCachedAccessRevalidatesAcceptedReference(t *testing.T) {
	f, st, _ := fixture(t)
	p := assetPath("image")
	e := acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
	if err := f.TouchManagedAccess(ctx, p); err != nil {
		t.Fatal(err)
	}
	m, err := st.Metadata(ctx, e.ID)
	if err != nil || m.LastBodyAccess.IsZero() {
		t.Fatal(m, err)
	}
	acceptAssets(t, st, "manual.md", "doc", "2", nil)
	if err := f.TouchManagedAccess(ctx, p); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatal("VFS cache hit bypasses current snapshot", err)
	}
}

func TestDocAssetsMaterializationPersistsAcrossStoreReopen(t *testing.T) {
	f, st, base := fixture(t)
	sc, err := st.Scope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base.docs["doc"] = `<p id="p">Hello</p><img id="i" src="image" name="picture.png" mime="image/png"/>`
	base.items = []larkcli.DriveItem{{Token: "doc", Name: "manual", Type: "docx"}}
	source := &assetSource{Source: base, body: "image content"}
	f.source = source
	listed(t, f)
	if _, err := f.MaterializeManaged(ctx, "manual.md"); err != nil {
		t.Fatal(err)
	}
	e, err := st.EntryByRemote(ctx, "manual.md")
	if err != nil {
		t.Fatal(err)
	}
	sn, err := st.Snapshot(ctx, e.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.ReadBlob(ctx, sn.BlockMap)
	if err != nil {
		t.Fatal(err)
	}
	blobPath := r.(interface{ Name() string }).Name()
	var sidecar model.DocSidecar
	err = json.NewDecoder(r).Decode(&sidecar)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sidecar.ProjectionVersion != 2 || len(sidecar.Assets) != 1 {
		t.Fatal(sidecar)
	}
	source.err = errors.New("offline after caching document")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(filepath.Dir(filepath.Dir(blobPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	raw, err := New(Dependencies{Store: reopened, Source: source, ProfileID: sc.ProfileID, MountID: sc.MountID})
	if err != nil {
		t.Fatal(err)
	}
	asset, err := raw.NewObject(ctx, assetPath("image"))
	if err != nil {
		t.Fatal(err)
	}
	if source.opens != 1 {
		t.Fatal("reopen performed an additional media download")
	}
	if body(t, asset) != "image content" {
		t.Fatal("reopened asset missing")
	}
}

func TestDocMediaStreamSizingAndCleanup(t *testing.T) {
	original := io.NopCloser(strings.NewReader("streamed media"))
	r, size, err := sizedDocMedia(ctx, original)
	if err != nil || size != 14 {
		t.Fatal(size, err)
	}
	filename := r.(interface{ Name() string }).Name()
	ranged, err := rangeReader(ctx, r, size, []fs.OpenOption{&fs.RangeOption{Start: -1, End: 5}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(ranged)
	if err != nil || string(got) != "media" {
		t.Fatal(string(got), err)
	}
	if err := ranged.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		t.Fatal("temporary read survived lease", err)
	}
}

func TestDocAssetsSidecarCannotGrantTokensAbsentFromOriginalXML(t *testing.T) {
	f, st, _ := fixture(t)
	p := assetPath("image")
	e := acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
	original, err := st.Snapshot(ctx, e.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		version int
		assets  []model.DocAsset
	}{
		{"forged canonical token", 2, []model.DocAsset{{Token: "unrelated", Path: assetPath("unrelated")}}},
		{"old projection", 1, []model.DocAsset{{Token: "image", Path: p}}},
		{"future projection", 3, []model.DocAsset{{Token: "image", Path: p}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sidecar, err := json.Marshal(model.DocSidecar{ProjectionVersion: tc.version, Assets: tc.assets})
			if err != nil {
				t.Fatal(err)
			}
			blockMap, err := st.PutBlob(ctx, bytes.NewReader(sidecar))
			if err != nil {
				t.Fatal(err)
			}
			current, err := st.Entry(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			sn := original
			sn.ID = ""
			sn.BlockMap = blockMap
			candidate, err := st.OfferRemote(ctx, e.ID, current.Generation, sn, sn.Markdown)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CommitPublication(ctx, candidate); err != nil {
				t.Fatal(err)
			}
			if _, err := f.NewObject(ctx, tc.assets[0].Path); !errors.Is(err, fs.ErrorObjectNotFound) {
				t.Fatal("sidecar granted unverified resource", err)
			}
		})
	}
}
