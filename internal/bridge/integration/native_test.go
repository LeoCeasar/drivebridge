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
		err = f.Sync()
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
	if err := os.Mkdir(point, 0700); err != nil {
		t.Fatal(err)
	}
	options := mountlib.Opt
	options.Daemon = false
	options.AllowOther = false
	options.AllowRoot = false
	options.DeviceName = "DriveBridge integration " + string(h.scope.MountID)
	_, unmount, actual, err := mount(h.vfs, point, &options)
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
	info, err := os.Stat(doc)
	if err != nil || info.Size() != int64(len(initial)) {
		t.Fatal(info, err)
	}
	got, err := os.ReadFile(doc)
	if err != nil || string(got) != string(initial) {
		t.Fatal(string(got), err)
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
	if err = f.Sync(); err != nil {
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
	checks := []string{"target-getattr-exact-size", "immutable-read", "os-write-fsync-close", "os-atomic-rename-keeps-token"}
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
		syncErr := f.Sync()
		closeErr := f.Close()
		h.io.fail.Store(false)
		if !errors.Is(syncErr, fault.want) {
			t.Fatalf("%s not returned through actual OS fsync: %v", fault.name, syncErr)
		}
		if closeErr == nil {
			t.Fatalf("%s was hidden by actual OS close", fault.name)
		}
		unchanged := h.entry("document.md")
		if unchanged.Generation != before.Generation || unchanged.Local != before.Local {
			t.Fatal("failed native sync accepted a generation")
		}
		retry, err := os.OpenFile(doc, os.O_RDWR|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = retry.Write(content); err != nil {
			t.Fatal(err)
		}
		if err = retry.Sync(); err != nil {
			t.Fatal(err)
		}
		if err = retry.Close(); err != nil {
			t.Fatal(err)
		}
		if got, err = os.ReadFile(doc); err != nil || string(got) != string(content) {
			t.Fatal(string(got), err)
		}
		checks = append(checks, "native-fsync-close-"+fault.name+"-and-retry")
	}
	h.cloud.mu.Lock()
	cloudMutations := h.cloud.applies + h.cloud.uploads + h.cloud.creates + h.cloud.deletes
	h.cloud.mu.Unlock()
	if cloudMutations != 0 {
		t.Fatal("native durability smoke unexpectedly performed cloud mutation")
	}
	sum := sha256.Sum256(got)
	writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", "native-"+string(h.scope.MountID)+".json", map[string]any{"schema_version": 1, "mode": "managed-overlay-synthetic-provider-native-driver", "goos": runtime.GOOS, "goarch": runtime.GOARCH, "engine": engine, "checks": checks, "final_sha256": hex.EncodeToString(sum[:]), "completed_at": time.Now().UTC(), "faults": "injected Store.SyncFile errors observed through kernel fsync/close, not a physically full disk"})
}
