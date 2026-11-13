//go:build cmount && windows

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/overlay"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/vfs"
	"golang.org/x/sys/windows"
)

// These gates use real Win32 calls through the production cmount mount method,
// with disposable Store/cache directories and an in-memory provider. They do
// not require credentials, an interactive desktop, or production cloud data.
func windowsNativeOptIn(t *testing.T) {
	t.Helper()
	optIn(t, "DRIVEBRIDGE_NATIVE", "DRIVEBRIDGE_REQUIRE_NATIVE")
	if os.Getenv("DRIVEBRIDGE_NATIVE_ALLOW_MOUNT") != "1" {
		t.Fatal("explicit DRIVEBRIDGE_NATIVE_ALLOW_MOUNT=1 required")
	}
}

func mountWindowsHarness(t *testing.T, h *harness) (string, func()) {
	t.Helper()
	_, mount := mountlib.ResolveMountMethod("cmount")
	if mount == nil {
		t.Fatal("required Windows cmount driver method missing; build with -tags cmount")
	}
	options := mountlib.Opt
	options.Daemon = false
	options.AllowOther = false
	options.AllowRoot = false
	options.VolumeName = "DriveBridge native save acceptance"
	options.DeviceName = "drivebridge-native-save:"
	// A long attribute lifetime exercises native invalidation on remote refresh.
	options.AttrTimeout = fs.Duration(time.Minute)
	mountedVFS := h.vfs
	exited, unmount, actual, err := mount(mountedVFS, "", &options)
	if err != nil {
		t.Fatalf("required WinFsp mount failed (driver absence is a failure): %v", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if err := unmount(); err != nil {
				t.Errorf("WinFsp unmount failed: %v", err)
			}
			if h.vfs == mountedVFS {
				h.vfs = nil // unmount already shuts this VFS down
			}
		})
	}
	t.Cleanup(stop)
	if len(actual) != 2 || actual[1] != ':' {
		t.Fatalf("expected an automatically assigned private drive letter, got %q", actual)
	}
	root := actual + `\`
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "document.md")); err == nil {
			return root, stop
		}
		select {
		case err := <-exited:
			t.Fatalf("WinFsp exited before native mount became ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("WinFsp mount did not become ready within 10 seconds")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func windowsNativeSave(t *testing.T, path, body string) time.Duration {
	t.Helper()
	start := time.Now()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal("native create/open:", err)
	}
	if _, err = io.WriteString(f, body); err != nil {
		_ = f.Close()
		t.Fatal("native write:", err)
	}
	syncErr := platform.SyncFile(f)
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		t.Fatalf("native FlushFileBuffers/close failed: sync=%v close=%v", syncErr, closeErr)
	}
	return time.Since(start)
}

func windowsNativeRead(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("native read %s: got %q, want %q, err=%v", filepath.Base(path), got, want, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(want)) {
		t.Fatalf("native size %s: got %v, want %d, err=%v", filepath.Base(path), info, len(want), err)
	}
}

func windowsNativeReplace(t *testing.T, from, to string) {
	t.Helper()
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING); err != nil {
		t.Fatal("native MoveFileExW replacement:", err)
	}
}

// MoveFileEx uses legacy Windows replacement semantics and cannot replace an
// open destination, even with FILE_SHARE_DELETE. FileRenameInfoEx explicitly
// requests POSIX semantics so the destination's old readers remain valid.
func windowsPosixReplace(t *testing.T, from, to string) {
	t.Helper()
	handle, err := windows.CreateFile(pForWindows(t, from), windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal("native POSIX rename source:", err)
	}
	defer windows.CloseHandle(handle)
	name, err := windows.UTF16FromString(to)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	offset := int(unsafe.Offsetof(header.FileName))
	buffer := make([]byte, max(int(unsafe.Sizeof(header)), offset+len(name)*2))
	binary.LittleEndian.PutUint32(buffer, windows.FILE_RENAME_REPLACE_IF_EXISTS|windows.FILE_RENAME_POSIX_SEMANTICS)
	binary.LittleEndian.PutUint32(buffer[unsafe.Offsetof(header.FileNameLength):], uint32((len(name)-1)*2))
	for n, c := range name {
		binary.LittleEndian.PutUint16(buffer[offset+n*2:], c)
	}
	if err = windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer))); err != nil {
		t.Fatal("native FileRenameInfoEx POSIX replacement:", err)
	}
}

func windowsNativeRenameControl(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	from, to := filepath.Join(root, "new.md"), filepath.Join(root, "old.md")
	windowsNativeSave(t, to, "NTFS original")
	reader := windowsSharedReader(t, to)
	windowsNativeSave(t, from, "NTFS replacement")
	if err := windows.MoveFileEx(pForWindows(t, from), pForWindows(t, to), windows.MOVEFILE_REPLACE_EXISTING); err == nil {
		t.Fatal("NTFS control unexpectedly admitted legacy replacement of an open destination")
	}
	windowsNativeRead(t, to, "NTFS original")
	windowsNativeRead(t, from, "NTFS replacement")
	windowsPosixReplace(t, from, to)
	windowsOldReader(t, reader, "NTFS original")
	windowsNativeRead(t, to, "NTFS replacement")
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func windowsSharedReader(t *testing.T, path string) *os.File {
	t.Helper()
	return windowsSharedOpen(t, path, windows.GENERIC_READ)
}

func windowsSharedOpen(t *testing.T, path string, access uint32) *os.File {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Go os.Open does not promise FILE_SHARE_DELETE. Request it explicitly so
	// Windows sharing checks admit an editor replacing/deleting this pathname.
	handle, err := windows.CreateFile(p, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal("native shared reader:", err)
	}
	f := os.NewFile(uintptr(handle), path)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func windowsOldReader(t *testing.T, reader *os.File, want string) {
	t.Helper()
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != want {
		t.Fatalf("old native reader: got %q, want %q, err=%v", got, want, err)
	}
}

// CloseHandle can return before WinFsp delivers Release. Poll the actual VFS
// activity instead of sleeping a fixed period or relaxing publication guards.
func windowsNativeIdle(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, w, ns := h.vfs.ManagedActivity()
		if r+w+ns == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("WinFsp did not release closed handles: readers=%d writers=%d namespace=%d", r, w, ns)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func windowsNativeEvidence(t *testing.T, h *harness, name string, fields map[string]any) {
	t.Helper()
	h.cloud.mu.Lock()
	mutations := h.cloud.applies + h.cloud.uploads + h.cloud.creates + h.cloud.deletes
	h.cloud.mu.Unlock()
	if mutations != 0 {
		t.Fatalf("native local acceptance unexpectedly mutated synthetic cloud: %d", mutations)
	}
	fields["schema_version"] = 1
	fields["driver"] = "WinFsp kernel / production cmount"
	fields["execution"] = "headless Windows OS APIs; no GUI editor acceptance"
	fields["provider"] = "synthetic; no credentials; no cloud mutations"
	fields["goos"], fields["goarch"] = runtime.GOOS, runtime.GOARCH
	fields["completed_at"] = time.Now().UTC()
	writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", name+"-"+string(h.scope.MountID)+".json", fields)
}

func TestWindowsNativeSaveParity(t *testing.T) {
	windowsNativeOptIn(t)
	t.Run("NTFS_rename_control", windowsNativeRenameControl)
	h := newHarness(t)
	h.read("document.md")
	original := h.entry("document.md")
	paused, release := pauseArchives(t, h)
	root, stop := mountWindowsHarness(t, h)
	doc := filepath.Join(root, "document.md")
	latest := "native first local checkpoint\n\nstable anchor 中文😀\n\ntail\n"
	windowsNativeSave(t, doc, latest)
	select {
	case <-paused.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic Store archival did not pause")
	}
	heldAt := time.Now()
	maxSave := time.Duration(0)
	const iterations = 100
	for n := 0; n < iterations; n++ {
		latest = fmt.Sprintf("native direct save %d\n\nstable anchor 中文😀\n\ntail\n", n)
		maxSave = max(maxSave, windowsNativeSave(t, doc, latest))
		latest = fmt.Sprintf("native safe replacement %d\n\nstable anchor 中文😀\n\ntail\n", n)
		temp := filepath.Join(root, fmt.Sprintf("editor-%03d.tmp", n))
		start := time.Now()
		windowsNativeSave(t, temp, latest)
		windowsNativeReplace(t, temp, doc)
		maxSave = max(maxSave, time.Since(start))
		windowsNativeRead(t, doc, latest)
		if _, err := os.ReadDir(root); err != nil {
			t.Fatal("native directory read:", err)
		}
	}
	old := latest
	reader := windowsSharedReader(t, doc)
	windowsOldReader(t, reader, old)
	latest = "native open-reader replacement\n\nstable anchor 中文😀\n\ntail\n"
	temp := filepath.Join(root, "open-reader.tmp")
	windowsNativeSave(t, temp, latest)
	windowsPosixReplace(t, temp, doc)
	windowsOldReader(t, reader, old)
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	windowsNativeRead(t, doc, latest)
	windowsNativeIdle(t, h)
	select {
	case <-paused.release:
		t.Fatal("archive resumed before native foreground saves completed")
	default:
	}
	held := time.Since(heldAt)
	// Shut down with the accepted local checkpoint still unarchived. This is
	// process restart/replay evidence, not a power-loss or hardware-fsync test.
	stop()
	release()
	if err := h.overlay.Shutdown(h.ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.open()
	root, _ = mountWindowsHarness(t, h)
	windowsNativeRead(t, filepath.Join(root, "document.md"), latest)
	windowsNativeIdle(t, h)
	current := h.entry("document.md")
	if current.ID != original.ID || current.Token != original.Token || h.blob(current.Local) != latest {
		t.Fatal("native restart lost resource identity or latest checkpoint")
	}
	sum := sha256.Sum256([]byte(latest))
	windowsNativeEvidence(t, h, "windows-native-save", map[string]any{
		"iterations": iterations, "direct_and_safe_saves": iterations * 2,
		"open_reader_retained_old_content_after_replacement": true,
		"open_reader_replacement_api":                        "FileRenameInfoEx REPLACE_IF_EXISTS|POSIX_SEMANTICS",
		"ntfs_legacy_and_posix_control_passed":               true,
		"restart_replayed_unarchived_checkpoint":             true, "token_preserved": true,
		"background_archive_held_seconds": held.Seconds(), "max_save_ms": float64(maxSave) / float64(time.Millisecond),
		"final_sha256": hex.EncodeToString(sum[:]),
		"fault_scope":  "background Store archive paused; local journal and physical disk available; orderly VFS stop/reopen, not power loss",
	})
}

type windowsDeleteObservation struct {
	Path      string `json:"path"`
	Readers   int    `json:"readers_at_namespace_commit"`
	Writers   int    `json:"writers_at_namespace_commit"`
	Namespace int    `json:"namespace_reservations"`
}

// Observe admission at the real managed backend boundary while keeping the
// production cmount host, native notifications and WinFsp callbacks unchanged.
type windowsDeleteObserver struct {
	*overlay.Fs
	v       *vfs.VFS
	mu      sync.Mutex
	removes []windowsDeleteObservation
}

func (o *windowsDeleteObserver) record(path string) {
	r, w, ns := o.v.ManagedActivity()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.removes = append(o.removes, windowsDeleteObservation{path, r, w, ns})
}
func (o *windowsDeleteObserver) BeginManagedRemove(ctx context.Context, path string) (string, error) {
	o.record(path)
	return o.Fs.BeginManagedRemove(ctx, path)
}
func (o *windowsDeleteObserver) Rmdir(ctx context.Context, path string) error {
	o.record(path)
	return o.Fs.Rmdir(ctx, path)
}

func TestWindowsNativeDeleteParity(t *testing.T) {
	windowsNativeOptIn(t)
	h := newHarness(t)
	options := h.vfs.Opt
	h.vfs.Shutdown()
	observer := &windowsDeleteObserver{Fs: h.overlay}
	var err error
	h.vfs, err = vfs.NewManaged(h.ctx, observer, &options)
	if err != nil {
		t.Fatal(err)
	}
	observer.v = h.vfs
	root, _ := mountWindowsHarness(t, h)
	file := filepath.Join(root, "data.bin")
	old := "ordinary native DeleteFile checkpoint"
	windowsNativeSave(t, file, old)
	windowsNativeIdle(t, h)
	h.entry("data.bin")
	// No application reader is open: any live reader observed during commit
	// belongs to WinFsp's own delete preflight, before its Release callback.
	p, _ := windows.UTF16PtrFromString(file)
	if err = windows.DeleteFile(p); err != nil {
		t.Fatal("ordinary native DeleteFileW failed:", err)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("DeleteFile pathname remains visible: %v", err)
	}
	windowsNativeIdle(t, h)
	dir := filepath.Join(root, "empty-directory")
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	windowsNativeIdle(t, h)
	p, _ = windows.UTF16PtrFromString(dir)
	if err = windows.RemoveDirectory(p); err != nil {
		t.Fatal("ordinary native RemoveDirectoryW failed:", err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("RemoveDirectory pathname remains visible: %v", err)
	}
	windowsNativeIdle(t, h)
	observer.mu.Lock()
	observations := append([]windowsDeleteObservation(nil), observer.removes...)
	observer.mu.Unlock()
	for _, path := range []string{"data.bin", "empty-directory"} {
		seen := false
		for _, observation := range observations {
			if observation.Path == path && observation.Readers > 0 && observation.Writers == 0 {
				seen = true
			}
		}
		if !seen {
			t.Fatalf("did not observe %s deletion before the driver's reader Release: %+v", path, observations)
		}
	}
	// Recreate, unlink with an explicit old reader, then recreate again. The
	// detached reader must retain bytes without resurrecting its old inode.
	windowsNativeSave(t, file, old)
	windowsNativeIdle(t, h)
	h.entry("data.bin")
	reader := windowsSharedReader(t, file)
	windowsOldReader(t, reader, old)
	if err = windows.DeleteFile(pForWindows(t, file)); err != nil {
		t.Fatal("native DeleteFile with shared reader:", err)
	}
	windowsOldReader(t, reader, old)
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("unlinked reader resurrected pathname: %v", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	windowsNativeIdle(t, h)
	newBody := "new native inode after delete"
	windowsNativeSave(t, file, newBody)
	windowsNativeRead(t, file, newBody)
	windowsNativeIdle(t, h)
	// FILE_SHARE_DELETE keeps the denial below meaningful: Windows sharing
	// flags permit deletion, but the managed namespace must protect a writer.
	writer := windowsSharedOpen(t, file, windows.GENERIC_READ|windows.GENERIC_WRITE)
	if err = windows.DeleteFile(pForWindows(t, file)); err == nil {
		t.Fatal("native deletion admitted an application writable handle")
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	windowsNativeRead(t, file, newBody)
	windowsNativeIdle(t, h)
	windowsNativeEvidence(t, h, "windows-native-delete", map[string]any{
		"ordinary_deletefile_before_driver_release": true, "empty_directory_before_driver_release": true,
		"delete_admission_observations": observations, "old_reader_preserved_after_unlink": true,
		"deleted_path_absent_before_recreate": true, "recreated_path_has_new_content": true,
		"writable_handle_protected_with_delete_sharing": true,
	})
}

func pForWindows(t *testing.T, path string) *uint16 {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWindowsNativeRemoteGeneration(t *testing.T) {
	windowsNativeOptIn(t)
	h := newHarness(t)
	root, _ := mountWindowsHarness(t, h)
	file := filepath.Join(root, "data.bin")
	initial := "\x00\xffinitial bytes"
	windowsNativeRead(t, file, initial)
	windowsNativeIdle(t, h)
	entry := h.entry("data.bin")
	versions := []string{"same-size-data!", "remote grown content with a different cache generation 中文😀", "tiny"}
	if len(versions[0]) != len(initial) {
		t.Fatal("same-size fixture has wrong length")
	}
	for _, body := range versions {
		h.cloud.setFile(h.fileToken, []byte(body))
		if err := h.engine.RefreshEntry(h.ctx, entry.ID); err != nil {
			t.Fatal("synthetic remote refresh:", err)
		}
		windowsNativeRead(t, file, body)
		windowsNativeIdle(t, h)
	}
	windowsNativeEvidence(t, h, "windows-native-generations", map[string]any{
		"same_size_refresh": true, "growing_refresh": true, "shrinking_refresh": true,
		"native_size_and_content_checked": true, "attr_timeout_seconds": 60,
	})
}
