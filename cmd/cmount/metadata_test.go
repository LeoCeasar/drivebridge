//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package cmount

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/winfsp/cgofuse/fuse"
)

type metadataTestFS struct {
	fs.Fs
	mu       sync.Mutex
	hydrated map[string]int
	opened   map[string]int
}
type metadataTestObject struct {
	fs.Object
	owner *metadataTestFS
	known bool
}

func (o *metadataTestObject) Size() int64 {
	if !o.known {
		return -1
	}
	return o.Object.Size()
}
func (o *metadataTestObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	o.owner.mu.Lock()
	o.owner.opened[o.Remote()]++
	o.owner.mu.Unlock()
	return o.Object.Open(ctx, options...)
}
func (f *metadataTestFS) object(o fs.Object) fs.Object {
	f.mu.Lock()
	known := f.hydrated[o.Remote()] > 0
	f.mu.Unlock()
	return &metadataTestObject{Object: o, owner: f, known: known}
}
func (f *metadataTestFS) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	entries, err := f.Fs.List(ctx, dir)
	for index, entry := range entries {
		if object, ok := entry.(fs.Object); ok {
			entries[index] = f.object(object)
		}
	}
	return entries, err
}
func (f *metadataTestFS) NewObject(ctx context.Context, name string) (fs.Object, error) {
	object, err := f.Fs.NewObject(ctx, name)
	if err != nil {
		return nil, err
	}
	return f.object(object), nil
}
func (f *metadataTestFS) MaterializeManaged(ctx context.Context, name string) (fs.Object, error) {
	object, err := f.Fs.NewObject(ctx, name)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.hydrated[name]++
	f.mu.Unlock()
	return &metadataTestObject{Object: object, owner: f, known: true}, nil
}
func (f *metadataTestFS) counts(name string) (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hydrated[name], f.opened[name]
}
func metadataFixture(t *testing.T) (*FS, *metadataTestFS, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	bodies := map[string]string{"cold.md": "# 完整冷文档\n\n段落与 emoji 😀\n", "sibling.pdf": "%PDF-1.7\nsynthetic binary fixture\n%%EOF\n"}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	base, err := fs.NewFs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	backend := &metadataTestFS{Fs: base, hydrated: map[string]int{}, opened: map[string]int{}}
	options := vfscommon.Opt
	options.CachePollInterval = 0
	managed, err := vfs.NewManaged(context.Background(), backend, &options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = managed.CleanUp(); managed.Shutdown() })
	return NewFS(managed, &mountlib.Options{}), backend, bodies
}

type metadataOnlyHandle struct {
	vfs.Handle
	node vfs.Node
}

func (h *metadataOnlyHandle) Node() vfs.Node { return h.node }

func TestManagedMetadataReaddirAndGetattrDoNotReadBodies(t *testing.T) {
	adapter, backend, bodies := metadataFixture(t)
	listed := map[string]bool{}
	if code := adapter.Readdir("/", func(name string, stat *fuse.Stat_t, _ int64) bool { listed[name] = true; return true }, 0, fhUnset); code != 0 {
		t.Fatal(code)
	}
	for name := range bodies {
		if !listed[name] {
			t.Fatal("missing metadata entry", name)
		}
		var stat fuse.Stat_t
		if code := adapter.Getattr("/"+name, &stat, fhUnset); code != 0 {
			t.Fatal(name, code)
		}
		if hydration, reads := backend.counts(name); hydration != 0 || reads != 0 {
			t.Fatalf("getattr fetched %s: hydration=%d reads=%d", name, hydration, reads)
		}
		if stat.Size != 0 {
			t.Fatalf("unknown metadata size must remain a placeholder: %d", stat.Size)
		}
	}
	// A metadata-only existing inode must not be hydrated by the fh Getattr path.
	node, err := adapter.VFS.StatMetadata("cold.md")
	if err != nil {
		t.Fatal(err)
	}
	fh := adapter.openHandle(&metadataOnlyHandle{node: node})
	defer adapter.closeHandle(fh)
	if code := adapter.Getattr("/cold.md", new(fuse.Stat_t), fh); code != 0 {
		t.Fatal(code)
	}
	if hydration, reads := backend.counts("cold.md"); hydration != 0 || reads != 0 {
		t.Fatal("fh getattr materialized body", hydration, reads)
	}
}

func TestManagedColdOpenHasExactSizeAndCompleteRead(t *testing.T) {
	for _, name := range []string{"cold.md", "sibling.pdf"} {
		t.Run(name, func(t *testing.T) {
			adapter, backend, bodies := metadataFixture(t)
			var before fuse.Stat_t
			if code := adapter.Getattr("/"+name, &before, fhUnset); code != 0 {
				t.Fatal(code)
			}
			if hydration, _ := backend.counts(name); hydration != 0 {
				t.Fatal("stat performed the download before Open")
			}
			fi := fuse.FileInfo_t{Flags: os.O_RDONLY}
			if code := adapter.OpenEx("/"+name, &fi); code != 0 {
				t.Fatal(code)
			}
			defer adapter.Release("/"+name, fi.Fh)
			if fi.DirectIo || fi.KeepCache {
				t.Fatal("managed cold Open must preserve normal I/O defaults", fi)
			}
			if !fi.PurgeAttr || !fi.PurgeUBC {
				t.Fatal("managed open must refresh kernel placeholder attributes and UBC", fi)
			}
			var opened fuse.Stat_t
			if code := adapter.Getattr("/"+name, &opened, fi.Fh); code != 0 {
				t.Fatal(code)
			}
			if opened.Size != int64(len(bodies[name])) {
				t.Fatalf("open size=%d want=%d", opened.Size, len(bodies[name]))
			}
			buffer := make([]byte, len(bodies[name])+100)
			count := adapter.Read("/"+name, buffer, 0, fi.Fh)
			if count != len(bodies[name]) || string(buffer[:count]) != bodies[name] {
				t.Fatalf("cold Read was truncated: count=%d want=%d", count, len(bodies[name]))
			}
			if count := adapter.Read("/"+name, buffer, int64(len(bodies[name])), fi.Fh); count != 0 {
				t.Fatal("expected EOF after exact content", count)
			}
			for other := range bodies {
				hydration, reads := backend.counts(other)
				if other == name {
					if hydration != 1 || reads < 1 {
						t.Fatal("selected body not read exactly once", hydration, reads)
					}
				} else if hydration != 0 || reads != 0 {
					t.Fatal("unopened sibling hydrated", other, hydration, reads)
				}
			}
		})
	}
}

func TestManagedWarmOpenStillRefreshesKernelSizeAndUBC(t *testing.T) {
	adapter, _, _ := metadataFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		fi := fuse.FileInfo_t{Flags: os.O_RDONLY}
		if code := adapter.OpenEx("/cold.md", &fi); code != 0 {
			t.Fatal(code)
		}
		if !fi.PurgeAttr || !fi.PurgeUBC || fi.DirectIo || fi.KeepCache {
			t.Fatalf("attempt%d did not request normal-I/O cache refresh: %+v", attempt, fi)
		}
		if code := adapter.Release("/cold.md", fi.Fh); code != 0 {
			t.Fatal(code)
		}
	}
}
func TestOrdinaryOpenDoesNotRequestManagedKernelPurges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ordinary"), []byte("ordinary"), 0600); err != nil {
		t.Fatal(err)
	}
	backend, err := fs.NewFs(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	options := vfscommon.Opt
	options.CachePollInterval = 0
	ordinary := vfs.New(context.Background(), backend, &options)
	t.Cleanup(func() { _ = ordinary.CleanUp(); ordinary.Shutdown() })
	adapter := NewFS(ordinary, &mountlib.Options{})
	fi := fuse.FileInfo_t{Flags: os.O_RDONLY}
	if code := adapter.OpenEx("/ordinary", &fi); code != 0 {
		t.Fatal(code)
	}
	defer adapter.Release("/ordinary", fi.Fh)
	if fi.PurgeAttr || fi.PurgeUBC {
		t.Fatalf("ordinary mount acquired managed-only purge policy: %+v", fi)
	}
}
