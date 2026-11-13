package overlay

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Cache reclamation tests assume that the separate durable history has accepted
// its inputs. Use the real archive, so source bytes are never simply discarded.
func archiveStoreHistory(t *testing.T, s *store.Store) {
	t.Helper()
	scope, err := s.Scope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := history.Open(ctx, history.Options{Dir: dir, ProfileID: scope.ProfileID, IdentityHash: scope.IdentityHash})
	if err != nil {
		t.Fatal(err)
	}
	for {
		state, err := s.HistoryStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.Pending == 0 {
			return
		}
		if _, err = repo.Ingest(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryRestoresOldDocumentImagesWithoutCloudDownload(t *testing.T) {
	f, s, base := fixture(t)
	source := &assetSource{Source: base, body: "old-image-payload"}
	f.source = source
	oldAsset := model.DocAsset{Token: "old-image", Name: "old.png", MIME: "image/png", Path: assetPath("old-image")}
	e := acceptAssets(t, s, "manual.md", "doc", "r1", []model.DocAsset{oldAsset})
	if err := f.ensureDocImages(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := f.TouchManagedAccess(ctx, "manual.md"); err != nil {
		t.Fatal(err)
	}
	scope, _ := s.Scope(ctx)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	r, err := history.Open(ctx, history.Options{Dir: dir, ProfileID: scope.ProfileID, IdentityHash: scope.IdentityHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Ingest(ctx, s); err != nil {
		t.Fatal(err)
	}
	page, _ := r.List(ctx, history.Query{EntryID: e.ID})
	var old history.Version
	for _, v := range page.Versions {
		if v.Event.RemoteSnapshot != nil && v.Event.RemoteSnapshot.Revision == "r1" && len(v.Media) > 0 {
			old = v
			break
		}
	}
	if old.ID == "" {
		t.Fatal("historical image bundle was not captured")
	}
	source.body = "new-image-payload"
	newAsset := model.DocAsset{Token: "new-image", Name: "new.png", MIME: "image/png", Path: assetPath("new-image")}
	e = acceptAssets(t, s, "manual.md", "doc", "r2", []model.DocAsset{newAsset})
	if err = f.ensureDocImages(ctx, e); err != nil {
		t.Fatal(err)
	}
	parent, err := s.EntryByRemote(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := r.OpenBody(ctx, old.ID, "local")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	bundle := &store.HistoryRestoreBundle{Snapshot: old.Event.RemoteSnapshot, Blobs: map[model.BlobID]io.Reader{}}
	for _, hash := range bundle.Snapshot.Blobs() {
		if hash == "" {
			continue
		}
		if _, ok := bundle.Blobs[hash]; ok {
			continue
		}
		role := ""
		for key, ref := range old.Bodies {
			if ref.SHA256 == string(hash) {
				role = key
				break
			}
		}
		if role == "" {
			for key, ref := range old.Media {
				if ref.SHA256 == string(hash) {
					role = "media:" + key
					break
				}
			}
		}
		reader, err := r.OpenBody(ctx, old.ID, role)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		bundle.Blobs[hash] = reader
	}
	req := model.HistoryRestoreRequest{ProfileID: scope.ProfileID, MountID: scope.MountID, EntryID: e.ID, VersionID: old.ID, RequestID: uuid.NewString(), ExpectedGeneration: e.Generation, ExpectedPath: e.Remote, ParentID: parent.ID, ParentGeneration: parent.Generation}
	result, err := s.RestoreHistory(ctx, req, body, bundle)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		extra := *bundle.Snapshot
		extra.ID = ""
		extra.Revision = fmt.Sprintf("extra-%d", i)
		if _, err = s.SaveSnapshot(ctx, extra); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.GC(ctx, store.GCOptions{Now: time.Now().Add(365 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	metadata, _ := s.Metadata(ctx, e.ID)
	if _, err = s.Snapshot(ctx, metadata.HistorySnapshot); err != nil {
		t.Fatal("restored asset snapshot was pruned", err)
	}
	before := source.opens
	source.err = errors.New("offline")
	image, err := f.NewObject(ctx, oldAsset.Path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := image.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(data) != "old-image-payload" || source.opens != before {
		t.Fatalf("restored image %q, fetches %d -> %d: %v", data, before, source.opens, err)
	}
	if !result.LocalSaved {
		t.Fatal(result)
	}
}
