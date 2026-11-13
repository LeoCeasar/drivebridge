//go:build cmount && darwin && cgo

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/internal/bridge/platform"
)

// TestNativeNonblockingLocalSave uses actual macFUSE kernel calls against a
// synthetic provider. The paused dependency is background Store archival, not
// local journal fsync, disk hardware or a real account.
func TestNativeNonblockingLocalSave(t *testing.T) {
	optIn(t, "DRIVEBRIDGE_NATIVE", "DRIVEBRIDGE_REQUIRE_NATIVE")
	if os.Getenv("DRIVEBRIDGE_NATIVE_ALLOW_MOUNT") != "1" {
		t.Fatal("explicit mount opt-in required")
	}
	h := newHarness(t)
	h.read("document.md")
	original := h.entry("document.md")
	paused, release := pauseArchives(t, h)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(home, "Shared", "DriveBridge-Save-Acceptance-"+uuid.NewString())
	if err = os.Mkdir(point, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(point)
	_, mount := mountlib.ResolveMountMethod("cmount")
	if mount == nil {
		t.Fatal("cmount unavailable")
	}
	options := mountlib.Opt
	options.Daemon = false
	options.AllowOther = false
	options.AllowRoot = false
	options.NoAppleXattr = true
	options.ExtraOptions = []string{"local"}
	options.VolumeName = "DriveBridge Save Acceptance"
	options.DeviceName = "drivebridge-save-acceptance:"
	mountErrors, unmount, actual, err := mount(h.vfs, point, &options)
	if err != nil {
		t.Fatal(err)
	}
	if actual != "" {
		point = actual
	}
	defer func() {
		release()
		if err := unmount(); err != nil {
			t.Error(err)
		}
		h.vfs = nil
	}()
	doc := filepath.Join(point, "document.md")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(doc); err == nil {
			break
		}
		select {
		case err := <-mountErrors:
			t.Fatalf("mount exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("native mount not ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var saveTimes, fsyncTimes, safeSaveTimes, readTimes, listTimes []time.Duration
	writeNative := func(file, content string) time.Duration {
		t.Helper()
		f, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(content)); err != nil {
			f.Close()
			t.Fatal(err)
		}
		start := time.Now()
		syncErr := platform.SyncFile(f)
		elapsed := time.Since(start)
		closeErr := f.Close()
		if syncErr != nil || closeErr != nil {
			t.Fatal("native save failed", syncErr, closeErr)
		}
		return elapsed
	}
	latest := "archive pause begins\n\nstable anchor 中文😀\n\ntail\n"
	writeNative(doc, latest)
	select {
	case <-paused.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive not paused")
	}
	heldAt := time.Now()
	for n := 0; n < 100; n++ {
		latest = fmt.Sprintf("native direct save %d\n\nstable anchor 中文😀\n\ntail\n", n)
		start := time.Now()
		fsyncTimes = append(fsyncTimes, writeNative(doc, latest))
		saveTimes = append(saveTimes, time.Since(start))
		latest = fmt.Sprintf("native safe save %d\n\nstable anchor 中文😀\n\ntail\n", n)
		start = time.Now()
		temp := filepath.Join(point, fmt.Sprintf("editor-%03d.tmp", n))
		writeNative(temp, latest)
		if err = os.Rename(temp, doc); err != nil {
			t.Fatal("native safe save rename failed", err)
		}
		safeSaveTimes = append(safeSaveTimes, time.Since(start))
		start = time.Now()
		body, err := os.ReadFile(doc)
		if err != nil || string(body) != latest {
			t.Fatalf("native read lost newest local save: %q err=%v", body, err)
		}
		readTimes = append(readTimes, time.Since(start))
		start = time.Now()
		if _, err = os.ReadDir(point); err != nil {
			t.Fatal("native readdir failed", err)
		}
		listTimes = append(listTimes, time.Since(start))
	}
	// A real editor can keep the previous inode open while it publishes an
	// atomic replacement. libfuse soft-removes that inode under .fuse_hidden*,
	// so this must preserve cloud identity as well as the old reader's bytes.
	reader, err := os.Open(doc)
	if err != nil {
		t.Fatal(err)
	}
	oldBody, err := io.ReadAll(reader)
	if err != nil {
		reader.Close()
		t.Fatal(err)
	}
	latest = "native open reader safe save\n\nstable anchor 中文😀\n\ntail\n"
	temp := filepath.Join(point, "open-reader.tmp")
	writeNative(temp, latest)
	if err = os.Rename(temp, doc); err != nil {
		reader.Close()
		t.Fatal("native open-reader replacement failed", err)
	}
	if _, err = reader.Seek(0, io.SeekStart); err != nil {
		reader.Close()
		t.Fatal(err)
	}
	oldAgain, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(oldAgain) != string(oldBody) {
		t.Fatalf("native old reader changed after replacement: %q want %q read=%v close=%v", oldAgain, oldBody, readErr, closeErr)
	}
	if body, err := os.ReadFile(doc); err != nil || string(body) != latest {
		t.Fatalf("native replacement not visible: %q err=%v", body, err)
	}
	select {
	case <-paused.release:
		t.Fatal("archive released before foreground acceptance completed")
	default:
	}
	interactive := os.Getenv("DRIVEBRIDGE_NATIVE_INTERACTIVE") == "1"
	if interactive {
		readyPath, releasePath := os.Getenv("DRIVEBRIDGE_NATIVE_READY_JSON"), os.Getenv("DRIVEBRIDGE_NATIVE_RELEASE_FILE")
		if !filepath.IsAbs(readyPath) || !filepath.IsAbs(releasePath) {
			t.Fatal("interactive ready and release control paths must be absolute")
		}
		ready, _ := json.MarshalIndent(map[string]any{"mount": point, "document": doc, "release_file": releasePath, "archive_held_since": heldAt.UTC(), "latest_body": latest}, "", "  ")
		if err = os.WriteFile(readyPath, append(ready, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		t.Logf("interactive native mount ready: %s", readyPath)
		deadline := time.Now().Add(15 * time.Minute)
		for {
			if _, err := os.Stat(releasePath); err == nil {
				break
			}
			select {
			case err := <-mountErrors:
				t.Fatalf("interactive mount exited: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("interactive native acceptance timed out")
			}
			time.Sleep(100 * time.Millisecond)
		}
		body, err := os.ReadFile(doc)
		if err != nil || len(body) == 0 {
			t.Fatal("interactive latest body unavailable", err)
		}
		latest = string(body)
	}
	if remaining := 30*time.Second - time.Since(heldAt); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case err := <-mountErrors:
			timer.Stop()
			t.Fatalf("mount exited while archive held: %v", err)
		}
	}
	held := time.Since(heldAt)
	release()
	if err = h.vfs.DrainManagedArchives(h.ctx); err != nil {
		t.Fatal(err)
	}
	current := h.entry("document.md")
	if current.ID != original.ID || current.Token != original.Token || h.blob(current.Local) != latest {
		actions, _ := h.store.NamespaceActions(h.ctx)
		namespaces := make([]any, 0, len(actions))
		for _, op := range actions {
			work, _ := h.store.NamespaceJournal(h.ctx, op.ID)
			namespaces = append(namespaces, work)
		}
		t.Logf("identity before=%+v after=%+v expected=%q actual=%q", original, current, latest, h.blob(current.Local))
		writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", "identity-failure.json", map[string]any{"original": original, "current": current, "expected": latest, "actual": h.blob(current.Local), "namespaces": namespaces})
		t.Fatal("native saves lost identity or newest bytes after archive resumed")
	}
	if err = h.engine.SyncEntry(h.ctx, current.ID); err != nil {
		t.Fatal("native newest save failed synthetic cloud synchronization", err)
	}
	if got := h.cloud.doc(t, h.docToken); got != latest {
		t.Fatalf("synthetic cloud did not receive newest local save: %q", got)
	}
	summary := func(values []time.Duration) map[string]float64 {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		return map[string]float64{"p50_ms": float64(values[49]) / float64(time.Millisecond), "p95_ms": float64(values[94]) / float64(time.Millisecond), "p99_ms": float64(values[98]) / float64(time.Millisecond), "max_ms": float64(values[99]) / float64(time.Millisecond)}
	}
	metrics := map[string]any{"fsync": summary(fsyncTimes), "fsync_save": summary(saveTimes), "safe_save": summary(safeSaveTimes), "warm_read": summary(readTimes), "readdir": summary(listTimes)}
	sum := sha256.Sum256([]byte(latest))
	writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", "nonblocking-native-"+string(h.scope.MountID)+".json", map[string]any{"schema_version": 1, "driver": "macFUSE kernel cmount/local", "provider": "synthetic; no credentials", "iterations": 100, "foreground_failures": 0, "background_archive_held_seconds": held.Seconds(), "latency": metrics, "token_preserved": true, "latest_body_archived_and_synced": true, "interactive": interactive, "final_sha256": hex.EncodeToString(sum[:]), "fault_scope": "background Store archive paused; local journal and physical disk remain available", "completed_at": time.Now().UTC()})
	t.Logf("native foreground latency with archive held %s: %+v", held, metrics)
}
