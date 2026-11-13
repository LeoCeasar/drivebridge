//go:build cmount && darwin && cgo

package integration

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/internal/bridge/model"
)

// Opt-in uses the real macFUSE kernel with the synthetic remote. No credentials
// or user cloud resources are touched, and this exact mount is cleaned on exit.
func TestNativePlainMarkdownMerge(t *testing.T) {
	optIn(t, "DRIVEBRIDGE_NATIVE", "DRIVEBRIDGE_REQUIRE_NATIVE")
	if os.Getenv("DRIVEBRIDGE_NATIVE_ALLOW_MOUNT") != "1" {
		t.Fatal("explicit mount opt-in required")
	}
	h := newHarness(t)
	addPlainMarkdown(h)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	point, err := os.MkdirTemp(filepath.Join(home, "Shared"), "DriveBridge-Markdown-Acceptance-")
	if err != nil {
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
	options.VolumeName = "DriveBridge Markdown Acceptance"
	options.DeviceName = "drivebridge-markdown-acceptance:"
	mountErrors, unmount, actualPoint, err := mount(h.vfs, point, &options)
	if err != nil {
		t.Fatal(err)
	}
	if actualPoint != "" {
		point = actualPoint
	}
	defer func() {
		if err := unmount(); err != nil {
			t.Error(err)
		}
		h.vfs = nil
	}()
	file := filepath.Join(point, plainMDName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(file); err == nil {
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
	b, err := os.ReadFile(file)
	if err != nil || string(b) != plainMDBase {
		t.Fatal(string(b), err)
	}
	h.cloud.setFile(plainMDToken, []byte("alpha one\nbeta remote extended text\n"))
	if err = os.WriteFile(file, []byte("alpha local\nbeta two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entry := h.entry(plainMDName)
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	want := "alpha local\nbeta remote extended text\n"
	b, err = os.ReadFile(file)
	if err != nil || string(b) != want {
		t.Fatalf("native merged body %q err=%v", b, err)
	}
	h.cloud.setFile(plainMDToken, []byte("alpha cloud collision\nbeta remote extended text\n"))
	if err = os.WriteFile(file, []byte("alpha local collision\nbeta remote extended text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(file); err != nil || string(body) != "alpha local collision\nbeta remote extended text\n" {
		t.Fatalf("native local journal read before archive: %q err=%v", body, err)
	}
	// OS close acknowledges the local journal. Explicitly join its background
	// Store handoff before asking the cloud engine to process this edit.
	entry = h.entry(plainMDName)
	if err = h.engine.SyncEntry(h.ctx, entry.ID); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	all, err := h.store.Conflicts(h.ctx)
	if err != nil || len(all) != 1 {
		t.Fatal(all, err)
	}
	c := all[0]
	chosen := []byte("alpha human resolved\nbeta remote extended text\n")
	if _, _, err = h.engine.Resolve(h.ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: chosen}); err != nil {
		t.Fatal(err)
	}
	if err = h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(file)
	if err != nil || string(b) != string(chosen) {
		t.Fatalf("native human body %q err=%v", b, err)
	}
	writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", "plain-markdown-native.json", map[string]any{"provider": "synthetic; no cloud mutations", "driver": "macFUSE kernel cmount/local", "cold_read": true, "independent_edit_merge": true, "merged_native_read_bytes": len(want), "overlap_conflict": true, "human_resolution": true, "resolved_native_read_bytes": len(chosen), "token_preserved": h.entry(plainMDName).Token == plainMDToken})
}
