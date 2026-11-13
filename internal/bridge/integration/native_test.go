package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/mount2"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/internal/bridge/platform"
)

func optIn(t *testing.T, flag, require string) {
	t.Helper()
	if os.Getenv(flag) == "1" {
		return
	}
	if os.Getenv(require) == "1" {
		t.Fatalf("required acceptance gate missing: set %s=1 only in an explicitly authorized environment", flag)
	}
	t.Skipf("UNVERIFIED: %s disabled; skipped tests are not native/live release evidence", flag)
}
func writeEvidence(t *testing.T, env, name string, v any) {
	t.Helper()
	dir := os.Getenv(env)
	if dir == "" {
		dir = t.TempDir()
	}
	if !filepath.IsAbs(dir) {
		t.Fatal("evidence path must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if os.IsNotExist(err) {
		if err = platform.EnsurePrivateDir(dir); err != nil {
			t.Fatal(err)
		}
		canonical, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = platform.EnsurePrivateDir(canonical); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(canonical, name)
	if _, err = os.Lstat(p); err == nil {
		t.Fatal("evidence destination already exists")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = platform.SyncFile(f)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if err = platform.SyncDir(canonical); err != nil {
		t.Fatal(err)
	}
	t.Logf("acceptance evidence: %s", p)
}

// TestNativeMount performs real kernel calls only under explicit opt-in. It uses
// a synthetic provider and a private temporary mount; it never uses an account.
func TestNativeMount(t *testing.T) {
	optIn(t, "DRIVEBRIDGE_NATIVE", "DRIVEBRIDGE_REQUIRE_NATIVE")
	if os.Getenv("DRIVEBRIDGE_NATIVE_ALLOW_MOUNT") != "1" {
		t.Fatal("explicit DRIVEBRIDGE_NATIVE_ALLOW_MOUNT=1 required")
	}
	engine := os.Getenv("DRIVEBRIDGE_NATIVE_ENGINE")
	if engine == "" {
		engine = "cmount"
		if runtime.GOOS == "linux" {
			engine = "mount2"
		}
	}
	if engine != "cmount" && engine != "mount2" {
		t.Fatal("unsupported native test engine")
	}
	_, mount := mountlib.ResolveMountMethod(engine)
	if mount == nil {
		t.Fatal("native mount method not compiled; use the appropriate build tag/platform, never count this as passed")
	}
	h := newHarness(t)
	point := filepath.Join(h.root, "native-mount")
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		point = filepath.Join(home, "Shared", "DriveBridge-Native-Acceptance-"+string(h.scope.MountID))
	}
	if err := os.Mkdir(point, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(point)
	options := mountlib.Opt
	options.Daemon = false
	options.AllowOther = false
	options.AllowRoot = false
	options.DeviceName = "DriveBridge integration " + string(h.scope.MountID)
	if runtime.GOOS == "darwin" {
		options.NoAppleXattr = true
		options.ExtraOptions = append(options.ExtraOptions, "local")
	}
	mountErrors, unmount, actual, err := mount(h.vfs, point, &options)
	if err != nil {
		t.Fatalf("native driver mount failed: %v", err)
	}
	defer func() {
		err := unmount()
		h.vfs = nil
		if err != nil {
			t.Errorf("native unmount failed: %v", err)
		}
	}()
	if actual != "" {
		point = actual
	}
	doc := filepath.Join(point, "document.md")
	initial := []byte("alpha beta gamma\n\nstable anchor 中文😀\n\ntail\n")
	deadline := time.Now().Add(10 * time.Second)
	var info os.FileInfo
	for {
		info, err = os.Stat(doc)
		if err == nil {
			break
		}
		select {
		case err := <-mountErrors:
			t.Fatalf("native mount exited before ready: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("native mount not ready", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.Size() < 0 {
		t.Fatal("cold placeholder has invalid size", info.Size())
	}
	h.cloud.mu.Lock()
	initialMutations := h.cloud.applies + h.cloud.uploads + h.cloud.creates + h.cloud.deletes
	h.cloud.mu.Unlock()
	if initialMutations != 0 {
		t.Fatal("cold metadata triggered cloud mutation")
	}
	got, err := os.ReadFile(doc)
	if err != nil || string(got) != string(initial) {
		t.Fatal(string(got), err)
	}
	info, err = os.Stat(doc)
	if err != nil || info.Size() != int64(len(initial)) {
		t.Fatal("hydrated getattr has incorrect size", info, err)
	}
	identity := h.entry("document.md")
	tmp := filepath.Join(point, "editor.tmp")
	body := []byte("native atomic replacement 😀\n")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(body); err != nil {
		t.Fatal(err)
	}
	if err = platform.SyncFile(f); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(tmp, doc); err != nil {
		t.Fatal(err)
	}
	after := h.entry("document.md")
	if after.ID != identity.ID || after.Token != identity.Token {
		t.Fatal("native replacement changed resource identity")
	}
	got, err = os.ReadFile(doc)
	if err != nil || string(got) != string(body) {
		t.Fatal(string(got), err)
	}
	checks := []string{"cold-placeholder-metadata", "cold-read-hydrates-exact-size", "immutable-read", "os-write-fsync-close", "os-atomic-rename-keeps-token"}
	for _, fault := range []struct {
		name string
		code int32
		want error
	}{{"EIO", 1, syscallEIO()}, {"ENOSPC", 2, syscallENOSPC()}} {
		before := h.entry("document.md")
		content := []byte("native retained after " + fault.name + "\n")
		f, err = os.OpenFile(doc, os.O_RDWR|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(content); err != nil {
			t.Fatal(err)
		}
		h.io.errno.Store(fault.code)
		h.io.fail.Store(true)
		syncErr := platform.SyncFile(f)
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil {
			t.Fatalf("%s background archive failure rejected local journal save: fsync=%v close=%v", fault.name, syncErr, closeErr)
		}
		if err = h.vfs.DrainManagedArchives(h.ctx); !errors.Is(err, fault.want) {
			t.Fatalf("%s background archive failure was not reported: %v", fault.name, err)
		}
		unchanged, err := h.store.EntryByRemote(h.ctx, "document.md")
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Generation != before.Generation || unchanged.Local != before.Local {
			t.Fatal("failed background archive changed accepted Store generation")
		}
		if got, err = os.ReadFile(doc); err != nil || string(got) != string(content) {
			t.Fatal("journal save unavailable while archive failed", string(got), err)
		}
		h.io.fail.Store(false)
		if err = h.vfs.DrainManagedArchives(h.ctx); err != nil {
			t.Fatal("background archive did not recover without rewriting", err)
		}
		accepted := h.entry("document.md")
		if accepted.ID != before.ID || accepted.Token != before.Token || h.blob(accepted.Local) != string(content) {
			t.Fatal("background archive recovery lost saved bytes or resource identity")
		}
		checks = append(checks, "native-local-fsync-close-success-background-archive-"+fault.name+"-and-retry-without-rewrite")
	}
	h.cloud.mu.Lock()
	cloudMutations := h.cloud.applies + h.cloud.uploads + h.cloud.creates + h.cloud.deletes
	h.cloud.mu.Unlock()
	if cloudMutations != 0 {
		t.Fatal("native durability smoke unexpectedly performed cloud mutation")
	}
	sum := sha256.Sum256(got)
	writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", "native-"+string(h.scope.MountID)+".json", map[string]any{"schema_version": 1, "mode": "managed-overlay-synthetic-provider-native-driver", "goos": runtime.GOOS, "goarch": runtime.GOARCH, "engine": engine, "checks": checks, "final_sha256": hex.EncodeToString(sum[:]), "completed_at": time.Now().UTC(), "faults": "injected background Store.SyncFile archive EIO/ENOSPC; native local journal fsync/close succeed; not a physically full disk"})
}
