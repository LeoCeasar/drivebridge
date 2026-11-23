package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

// TestNativeDirectoryFreshness requires actual kernel enumeration after the
// production overlay adopts and notifies a directory snapshot. No account is used.
func TestNativeDirectoryFreshness(t *testing.T) {
	optIn(t, "DRIVEBRIDGE_NATIVE", "DRIVEBRIDGE_REQUIRE_NATIVE")
	if os.Getenv("DRIVEBRIDGE_NATIVE_ALLOW_MOUNT") != "1" {
		t.Fatal("explicit mount opt-in required")
	}
	h := newHarness(t)
	h.cloud.mu.Lock()
	h.cloud.resources["parent"] = resource{kind: model.KindFolder, name: "Parent", parent: h.scope.FolderToken}
	for i := range 11 {
		name := fmt.Sprintf("old-%02d", i)
		h.cloud.resources[name] = resource{kind: model.KindFolder, name: name, parent: "parent"}
	}
	h.cloud.mu.Unlock()
	h.vfs.Opt.DirCacheTime = fs.Duration(30 * time.Second)
	_, mount := mountlib.ResolveMountMethod("cmount")
	if mount == nil {
		t.Fatal("required cmount driver is not compiled")
	}
	point := ""
	if runtime.GOOS != "windows" {
		base := h.root
		if runtime.GOOS == "darwin" {
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			base = filepath.Join(home, "Shared")
		}
		point = filepath.Join(base, "DriveBridge-Directory-Acceptance-"+string(h.scope.MountID))
		if err := os.Mkdir(point, 0700); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(point)
	}
	options := mountlib.Opt
	options.Daemon = false
	options.AllowOther = false
	options.AllowRoot = false
	options.AttrTimeout = fs.Duration(time.Minute)
	options.VolumeName = "DriveBridge Directory Acceptance"
	options.DeviceName = "drivebridge-directory-acceptance:"
	if runtime.GOOS == "darwin" {
		options.NoAppleXattr = true
		options.ExtraOptions = []string{"local"}
	}
	exited, unmount, actual, err := mount(h.vfs, point, &options)
	if err != nil {
		t.Fatal("required native mount failed:", err)
	}
	defer func() {
		if err := unmount(); err != nil {
			t.Error(err)
		}
		h.vfs = nil
	}()
	if actual != "" {
		point = actual
	}
	if runtime.GOOS == "windows" {
		point = strings.TrimRight(point, `\`) + `\`
	}
	parent := filepath.Join(point, "Parent")
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := os.ReadDir(parent)
		if err == nil && len(rows) == 11 {
			break
		}
		select {
		case err := <-exited:
			t.Fatal("mount exited", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("initial native parent not visible", len(rows), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.cloud.mu.Lock()
	fetched, observed := h.cloud.fetches, h.cloud.observations
	h.cloud.resources["new-folder"] = resource{kind: model.KindFolder, name: "Open平台渠道协议与分销功能", parent: "parent"}
	h.cloud.mu.Unlock()
	// A fresh snapshot stays local. After expiry, a real kernel directory
	// enumeration must enqueue the automatic request; no manual request is made.
	life, cancel := context.WithCancel(h.ctx)
	workerDone := make(chan struct{})
	workerErr := make(chan error, 1)
	go func() {
		defer close(workerDone)
		for {
			select {
			case <-life.Done():
				return
			case <-h.overlay.DirectoryRefreshWake():
				if err := h.overlay.RefreshRequestedDirectories(life, h.vfs.NotifyManagedDirectoryChanged); err != nil {
					select {
					case workerErr <- err:
					default:
					}
				}
			}
		}
	}()
	defer func() { cancel(); <-workerDone }()
	time.Sleep(31 * time.Second)
	deadline = time.Now().Add(10 * time.Second)
	for {
		rows, err := os.ReadDir(parent)
		found := false
		for _, row := range rows {
			if row.Name() == "Open平台渠道协议与分销功能" && row.IsDir() {
				found = true
			}
		}
		if err == nil && len(rows) == 12 && found {
			break
		}
		select {
		case readErr := <-workerErr:
			t.Fatal("automatic directory refresh failed", readErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("expired native parent did not automatically discover cloud folder", len(rows), err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.cloud.mu.Lock()
	unchanged := h.cloud.fetches == fetched && h.cloud.observations == observed && h.cloud.creates == 0 && h.cloud.deletes == 0
	h.cloud.mu.Unlock()
	if !unchanged {
		t.Fatal("directory discovery acquired bodies or mutated cloud data")
	}
	writeEvidence(t, "DRIVEBRIDGE_NATIVE_EVIDENCE_DIR", "native-directory-"+string(h.scope.MountID)+".json", map[string]any{"platform": runtime.GOOS, "before_children": 11, "after_children": 12, "kernel_enumeration": true, "native_attribute_timeout_seconds": 60, "metadata_only": true, "automatic_native_reopen": true, "provider": "synthetic"})
}
