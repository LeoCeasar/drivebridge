package overlay

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/vfs"
)

func TestDocAssetsManagedVFSCacheLifecycle(t *testing.T) {
	f, st, base, managed, _ := bodyCacheIntegration(t, nil)
	source := &assetSource{Source: base, body: "real image bytes"}
	f.source = source
	p := assetPath("image")
	acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
	node, err := managed.StatMetadata(p)
	if err != nil {
		t.Fatal(err)
	}
	if node.DirEntry().Size() != -1 || source.opens != 0 {
		t.Fatal("metadata fetched media", node.DirEntry().Size(), source.opens)
	}
	first, err := managed.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(first)
	if err != nil || string(data) != source.body {
		t.Fatal(string(data), err)
	}
	if source.opens != 1 {
		t.Fatal("expected one durable image download", source.opens)
	}
	called := false
	err = managed.TryExternalUpdate(ctx, p, func() (fs.Object, error) { called = true; return f.NewObject(ctx, p) })
	if !errors.Is(err, vfs.ErrExternalUpdateBusy) || called {
		t.Fatal("open media cache evicted", called, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if managed.ManagedCacheBytes() != int64(len(source.body)) {
		t.Fatal(managed.ManagedCacheBytes())
	}
	source.err = errors.New("network must not be used on cache hit")
	second, err := managed.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(second)
	if err != nil || string(data) != source.body {
		t.Fatal(string(data), err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if source.opens != 1 {
		t.Fatal("VFS missed cache", source.opens)
	}
	acceptAssets(t, st, "manual.md", "doc", "2", nil)
	if handle, err := managed.Open(p); err == nil {
		handle.Close()
		t.Fatal("cached media bypassed revoked reference")
	}
}

func TestDocAssetsManagedVFSRejectsWriteAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flags  int
		forged bool
	}{
		{name: "truncate", flags: os.O_WRONLY | os.O_TRUNC},
		{name: "read write", flags: os.O_RDWR},
		{name: "create child", flags: os.O_WRONLY | os.O_CREATE, forged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, st, base, managed, _ := bodyCacheIntegration(t, nil)
			source := &assetSource{Source: base, body: "original image"}
			f.source = source
			p := assetPath("image")
			acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
			remote := p
			if tc.forged {
				remote = ".drivebridge-assets/forged.png"
			}
			handle, err := managed.OpenFile(remote, tc.flags, 0600)
			if handle != nil {
				handle.Close()
			}
			if !errors.Is(err, os.ErrPermission) {
				t.Fatal("write admitted", err)
			}
			if source.opens != 0 || managed.ManagedCacheBytes() != 0 || managed.ManagedCachePending() {
				t.Fatal("denied write fetched or dirtied cache", source.opens, managed.ManagedCacheBytes(), managed.ManagedCachePending())
			}
			jobs, err := st.Pending(ctx)
			if err != nil || len(jobs) != 0 {
				t.Fatal("denied write queued", jobs, err)
			}
			if tc.forged {
				if node, err := managed.StatMetadata(remote); node != nil || (!errors.Is(err, os.ErrNotExist) && !errors.Is(err, fs.ErrorObjectNotFound)) {
					t.Fatal("forged node survived", err)
				}
			}
		})
	}
}

func TestDocAssetsManagedVFSRejectsDirectNodeWrites(t *testing.T) {
	_, st, _, managed, _ := bodyCacheIntegration(t, nil)
	p := assetPath("image")
	acceptAssets(t, st, "manual.md", "doc", "1", []model.DocAsset{{Token: "image", Path: p}})
	asset, err := managed.StatMetadata(p)
	if err != nil {
		t.Fatal(err)
	}
	if handle, err := asset.Open(os.O_RDWR); handle != nil || !errors.Is(err, os.ErrPermission) {
		if handle != nil {
			handle.Close()
		}
		t.Fatal("direct file write admitted", err)
	}
	node, err := managed.StatMetadata(".drivebridge-assets")
	if err != nil {
		t.Fatal(err)
	}
	if file, err := node.(*vfs.Dir).Create("forged.png", os.O_CREATE|os.O_WRONLY); file != nil || !errors.Is(err, os.ErrPermission) {
		t.Fatal("direct create admitted", file, err)
	}
	if managed.ManagedCachePending() {
		t.Fatal("direct create dirtied cache")
	}
}
