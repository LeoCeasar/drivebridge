//go:build cmount && windows

package cmount

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/winfsp/cgofuse/fuse"
)

// callRecorder wraps FS to record the order of the FUSE callbacks WinFsp makes.
type callRecorder struct {
	*FS
	calls chan string
}

func (r *callRecorder) record(s string) {
	select {
	case r.calls <- s:
	default:
	}
}
func (r *callRecorder) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	rc := r.FS.Getattr(path, stat, fh)
	if fh == fhUnset {
		r.record("getattr " + path)
	}
	return rc
}
func (r *callRecorder) OpenEx(path string, fi *fuse.FileInfo_t) int {
	r.record("open " + path)
	return r.FS.OpenEx(path, fi)
}
func (r *callRecorder) Readdir(p string, fill func(string, *fuse.Stat_t, int64) bool, o int64, fh uint64) int {
	return r.FS.Readdir(p, func(name string, st *fuse.Stat_t, ofst int64) bool {
		if st == nil && name != "." && name != ".." {
			r.record("readdir-nil-stat " + name)
		}
		return fill(name, st, ofst)
	}, o, fh)
}

// mountSynthetic mounts a managed VFS (temporary backing + cache dirs only)
// whose objects report an unknown size until materialized, on a free drive
// letter, through the real WinFsp stack.
func mountSynthetic(t *testing.T, attrTimeout time.Duration) (root string, backend *metadataTestFS, bodies map[string]string, calls chan string) {
	t.Helper()
	if err := config.SetCacheDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bodies = map[string]string{
		"cold.md":      "# 完整冷文档\n\n段落与 emoji 😀\n" + string(make([]byte, 15000)),
		"sibling.pdf":  "%PDF-1.7\nsynthetic binary fixture\n%%EOF\n",
		"third.txt":    "third file body\n",
		"stat.md":      "# stat only\n" + string(make([]byte, 2048)),
		"readfile.bin": string(make([]byte, 40000)),
		"broken.md":    "never materializes\n",
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base, err := fs.NewFs(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	backend = &metadataTestFS{Fs: base, hydrated: map[string]int{}, opened: map[string]int{}}
	options := vfscommon.Opt
	options.CachePollInterval = 0
	managed, err := vfs.NewManaged(context.Background(), backend, &options)
	if err != nil {
		t.Fatal(err)
	}

	// Mount like mount() does, but around a recording FS.
	opt := mountlib.Opt
	opt.AttrTimeout = fs.Duration(attrTimeout)
	calls = make(chan string, 4096)
	recorder := &callRecorder{FS: NewFS(managed, &opt), calls: calls}
	host := fuse.NewFileSystemHost(recorder)
	host.SetCapReaddirPlus(true)
	host.SetCapCaseInsensitive(true)
	mountpoint, err := getUnusedDrive()
	if err != nil {
		managed.Shutdown()
		t.Skipf("no free drive letter: %v", err)
	}
	errc := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				errc <- errors.New("WinFsp unavailable")
			}
		}()
		if !host.Mount(mountpoint, mountOptions(managed, "", mountpoint, &opt)) {
			errc <- errors.New("mount failed")
		}
	}()
	select {
	case err := <-errc:
		managed.Shutdown()
		t.Skipf("WinFsp mount unavailable: %v", err)
	case <-recorder.ready:
	}
	root = mountpoint + `\`
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(root); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mount point never became available")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		time.Sleep(200 * time.Millisecond) // let WinFsp deliver Release for closed handles
		if !host.Unmount() {
			t.Error("unmount failed")
		}
		managed.Shutdown()
	})
	return root, backend, bodies, calls
}

func drainCalls(calls chan string) (out []string) {
	for {
		select {
		case c := <-calls:
			out = append(out, c)
		default:
			return out
		}
	}
}

func hydrationOf(backend *metadataTestFS, name string) int {
	h, _ := backend.counts(name)
	return h
}

// TestWindowsMountColdFirstOpenReportsRealSize is the regression test for the
// first-open FileSize=0 bug: WinFsp answers CreateFile with the result of a
// path-based getattr made before open, so that getattr must carry the real
// size, while listing a directory must stay download-free.
func TestWindowsMountColdFirstOpenReportsRealSize(t *testing.T) {
	// A long attribute timeout proves a stale placeholder size cannot be
	// cached from the listing and leak into the first open.
	root, backend, bodies, calls := mountSynthetic(t, time.Minute)

	t.Run("listing does not materialize", func(t *testing.T) {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != len(bodies) {
			t.Fatalf("listed %d entries, want %d", len(entries), len(bodies))
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() != 0 {
				t.Errorf("%s: listing should show the placeholder size, got %d", entry.Name(), info.Size())
			}
		}
		for name := range bodies {
			if h, r := backend.counts(name); h != 0 || r != 0 {
				t.Errorf("listing materialized %s: hydration=%d reads=%d", name, h, r)
			}
		}
		for _, c := range drainCalls(calls) {
			if strings.HasPrefix(c, "readdir") {
				t.Errorf("entry listed without stat, WinFsp would getattr (download) it: %s", c)
			}
		}
	})

	t.Run("GetFileAttributes reports real size", func(t *testing.T) {
		info, err := os.Stat(root + "stat.md")
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != int64(len(bodies["stat.md"])) {
			t.Fatalf("size=%d want=%d", info.Size(), len(bodies["stat.md"]))
		}
		if hydrationOf(backend, "stat.md") != 1 {
			t.Fatal("stat.md hydrations", hydrationOf(backend, "stat.md"))
		}
	})

	t.Run("first open handle reports real size and reads exactly", func(t *testing.T) {
		const name = "cold.md"
		want := bodies[name]
		drainCalls(calls)
		f, err := os.Open(root + name)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		// FileSize of the handle as seen via GetFileInformationByHandle.
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != int64(len(want)) {
			t.Fatalf("first-open handle size=%d want=%d", info.Size(), len(want))
		}
		got, err := io.ReadAll(f)
		if err != nil || string(got) != want {
			t.Fatalf("read %d bytes err=%v want %d", len(got), err, len(want))
		}
		// Document the WinFsp contract the fix relies on: getattr(path)
		// happens before open and is not repeated afterwards.
		order := drainCalls(calls)
		getattrBeforeOpen, openSeen := false, false
		for _, c := range order {
			if c == "open /"+name {
				openSeen = true
			} else if c == "getattr /"+name && !openSeen {
				getattrBeforeOpen = true
			}
		}
		if !getattrBeforeOpen || !openSeen {
			t.Errorf("unexpected WinFsp call order: %v", order)
		}
		if hydrationOf(backend, name) != 1 {
			t.Fatal("hydrations", hydrationOf(backend, name))
		}
	})

	t.Run("os.ReadFile of a cold file", func(t *testing.T) {
		got, err := os.ReadFile(root + "readfile.bin")
		if err != nil || len(got) != len(bodies["readfile.bin"]) {
			t.Fatalf("read %d bytes err=%v want %d", len(got), err, len(bodies["readfile.bin"]))
		}
	})

	t.Run("second open stays consistent", func(t *testing.T) {
		f, err := os.Open(root + "cold.md")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if info, err := f.Stat(); err != nil || info.Size() != int64(len(bodies["cold.md"])) {
			t.Fatal(info, err)
		}
		if hydrationOf(backend, "cold.md") != 1 {
			t.Fatal("warm open re-materialized the body")
		}
	})

	t.Run("untouched files stay cold", func(t *testing.T) {
		for _, name := range []string{"sibling.pdf", "third.txt", "broken.md"} {
			if h, r := backend.counts(name); h != 0 || r != 0 {
				t.Errorf("%s materialized without being opened: hydration=%d reads=%d", name, h, r)
			}
		}
	})

	t.Run("materialization failure is an error, never size 0", func(t *testing.T) {
		backend.mu.Lock()
		backend.failMaterialize = syscall.EIO
		backend.mu.Unlock()
		if info, err := os.Stat(root + "broken.md"); err == nil {
			t.Errorf("stat of a file that cannot be materialized succeeded with size=%d", info.Size())
		}
		if f, err := os.Open(root + "broken.md"); err == nil {
			info, _ := f.Stat()
			f.Close()
			t.Errorf("open of a file that cannot be materialized succeeded (size=%v)", info)
		}
		backend.mu.Lock()
		backend.failMaterialize = nil
		backend.mu.Unlock()
		got, err := os.ReadFile(root + "broken.md")
		if err != nil || string(got) != bodies["broken.md"] {
			t.Fatalf("recovered read: %q err=%v", got, err)
		}
	})
}
