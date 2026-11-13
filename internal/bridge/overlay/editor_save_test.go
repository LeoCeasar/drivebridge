package overlay

import (
	"context"
	"errors"
	"fmt"
	"github.com/rclone/rclone/internal/bridge/store"
	"io"
	"os"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestEditorTildeSaveRetainsIdentityAcrossRepeatedPendingSaves(t *testing.T) {
	for _, backup := range []string{"manual~.md", "manual.md~"} {
		t.Run(backup, func(t *testing.T) {
			f, st, source := fixture(t)
			en := acceptAssets(t, st, "manual.md", "stable-doc", "1", nil)
			source.bodyErr = errors.New("offline; saving must not contact cloud")
			for i := 1; i <= 3; i++ {
				current, err := f.NewObject(ctx, "manual.md")
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.Move(ctx, current, backup); err != nil {
					t.Fatal("local editor backup failed", i, err)
				}
				saved := put(t, f, "manual.md", fmt.Sprintf("latest local %d", i))
				got, err := st.EntryByRemote(ctx, "manual.md")
				if err != nil {
					t.Fatal(err)
				}
				if got.ID != en.ID || got.Token != en.Token || got.RemoteCheckpoint != en.RemoteCheckpoint {
					t.Fatal("editor save lost original cloud identity")
				}
				if body(t, saved) != fmt.Sprintf("latest local %d", i) || !f.LocalOnly(backup) {
					t.Fatal("local save not retained or backup queued for upload")
				}
				if i == 1 {
					jobs, err := st.Pending(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err = st.Prepare(ctx, jobs[0].ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if source.fetches != 0 || source.observes != 0 {
				t.Fatal("save performed network reads")
			}
			if err := st.Validate(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEditorTildeSaveDoesNotWaitForDirectoryNetworkRefresh(t *testing.T) {
	f, st, _ := fixture(t)
	acceptAssets(t, st, "manual.md", "stable-doc", "1", nil)
	root, err := st.EntryByRemote(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	f.refreshing[root.ID] = &directoryRefresh{remote: "", done: make(chan struct{})}
	// The refresh channel is never closed, so waiting on it still fails this
	// test. Allow local durable transactions to finish under race/CI disk load.
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	current, err := f.NewObject(short, "manual.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Move(short, current, "manual~.md"); err != nil {
		t.Fatal("save waited for cloud directory listing", err)
	}
}

func TestReadWriteSaveDoesNotFetchDocumentImages(t *testing.T) {
	f, st, base, managed, _ := bodyCacheIntegration(t, nil)
	source := &assetSource{Source: base, err: errors.New("image network is offline")}
	f.source = source
	acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	h, err := managed.OpenFile("manual.md", os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal("local save blocked by image prefetch", err)
	}
	if _, err = h.Write([]byte("latest local")); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if source.opens != 0 {
		t.Fatal("write-only activity fetched images")
	}
	en, err := st.EntryByRemote(ctx, "manual.md")
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.ReadBlob(ctx, en.Local)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

func TestManagedEditorSaveNeverRestoresOldCloudBody(t *testing.T) {
	f, st, base, managed, _ := bodyCacheIntegration(t, nil)
	en := acceptAssets(t, st, "manual.md", "stable-doc", "1", nil)
	base.bodyErr = errors.New("network unavailable")
	for i := 1; i <= 3; i++ {
		if err := managed.Rename("manual.md", "manual~.md"); err != nil {
			t.Fatal("editor backup failed", i, err)
		}
		h, err := managed.OpenFile("manual.md", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		text := fmt.Sprintf("latest local body %d\n", i)
		if _, err = h.Write([]byte(text)); err != nil {
			t.Fatal(err)
		}
		if err = h.Close(); err != nil {
			t.Fatal(err)
		}
		current, err := st.EntryByRemote(ctx, "manual.md")
		if err != nil || current.ID != en.ID || current.Token != en.Token {
			t.Fatal("identity lost", current, err)
		}
		old, err := st.Snapshot(ctx, en.RemoteCheckpoint)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.OfferRemote(ctx, en.ID, current.Generation, old, old.Markdown); !errors.Is(err, store.ErrBusy) {
			t.Fatal("cloud observation can replace pending local save", err)
		}
		r, err := managed.Open("manual.md")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(data) != text {
			t.Fatal("latest local body rolled back", string(data), err)
		}
	}
	if err := st.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	_ = f
}

func TestWarmNewFileLookupDoesNotDependOnUnrelatedImageMetadata(t *testing.T) {
	f, st, _ := fixture(t)
	en := acceptAssets(t, st, "other.md", "other-doc", "1", []model.DocAsset{{Token: "image", Path: assetPath("image")}})
	root, err := st.EntryByRemote(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = st.MarkDirectoryCached(ctx, root); err != nil {
		t.Fatal(err)
	}
	sn, err := st.Snapshot(ctx, en.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.ReadBlob(ctx, sn.BlockMap)
	if err != nil {
		t.Fatal(err)
	}
	name := r.(interface{ Name() string }).Name()
	r.Close()
	// A corrupt unrelated projection must not turn a cached negative filename
	// lookup into an expensive/full directory render or prevent a new local save.
	if err = os.WriteFile(name, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.NewObject(ctx, "new-save.txt"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatal("unrelated document metadata blocked new local filename", err)
	}
}
