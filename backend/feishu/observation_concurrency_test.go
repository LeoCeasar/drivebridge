package feishu

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/larkcli"
)

// This controlled process uses local Unix IPC to hold a completed download while
// an upload reaches transport completion. It never invokes the official CLI.
func TestNativeObservationProcess(t *testing.T) {
	root := os.Getenv("DB_NATIVE_TEST_ROOT")
	if root == "" {
		return
	}
	args := os.Args
	has := func(want string) bool {
		for _, a := range args {
			if a == want {
				return true
			}
		}
		return false
	}
	emit := func(data any) {
		json.NewEncoder(os.Stdout).Encode(struct {
			OK       bool   `json:"ok"`
			Identity string `json:"identity"`
			Data     any    `json:"data"`
		}{true, "user", data})
	}
	if has("+download") {
		b, err := os.ReadFile(filepath.Join(root, "state"))
		if err != nil {
			os.Exit(2)
		}
		if os.WriteFile("download", b, 0600) != nil {
			os.Exit(2)
		}
		conn, err := net.Dial("unix", filepath.Join(root, "barrier"))
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(conn, "download")
		if _, err = bufio.NewReader(conn).ReadString('\n'); err != nil {
			os.Exit(2)
		}
		conn.Close()
		emit(map[string]any{"saved_path": "download", "size_bytes": len(b)})
	} else if has("+upload") {
		b, err := os.ReadFile("input")
		if err != nil {
			os.Exit(2)
		}
		if os.WriteFile(filepath.Join(root, "state"), b, 0600) != nil {
			os.Exit(2)
		}
		emit(map[string]any{"file_token": "file", "size": len(b)})
	} else {
		os.Exit(2)
	}
	os.Exit(0)
}

type nativeOperationContextKey struct{}
type nativeBarrierCoordinator struct {
	uploadDone chan struct{}
	once       sync.Once
}

func (c *nativeBarrierCoordinator) WithCredentialLocks(ctx context.Context, a, u string, run func() error) error {
	err := run()
	if ctx.Value(nativeOperationContextKey{}) == "update" {
		c.once.Do(func() { close(c.uploadDone) })
	}
	return err
}
func (c *nativeBarrierCoordinator) WithEnrollmentLock(ctx context.Context, run func() error) error {
	return run()
}

func TestNativeOpenUpdateBarrierPreservesCapturedReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("local Unix IPC fixture; Windows compile coverage remains separate")
	}
	root, err := os.MkdirTemp("", "db-f3-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err = os.WriteFile(filepath.Join(root, "state"), []byte("old-captured-snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "barrier"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	downloadReady := make(chan struct{})
	releaseDownload := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(releaseDownload) })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || line != "download\n" {
					return
				}
				select {
				case downloadReady <- struct{}{}:
				case <-ctx.Done():
					return
				}
				select {
				case <-releaseDownload:
					fmt.Fprintln(conn, "continue")
				case <-ctx.Done():
				}
			}()
		}
	}()
	executable, _ := os.Executable()
	script := filepath.Join(root, "cli")
	text := "#!/bin/sh\nDB_NATIVE_TEST_ROOT='" + strings.ReplaceAll(root, "'", "'\\''") + "' exec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=TestNativeObservationProcess -- \"$@\"\n"
	if err = os.WriteFile(script, []byte(text), 0700); err != nil {
		t.Fatal(err)
	}
	coordinator := &nativeBarrierCoordinator{uploadDone: make(chan struct{})}
	runner, err := larkcli.NewManaged(larkcli.ManagedOptions{Executable: script, AuthRef: "ref", AuthRoot: t.TempDir(), Identity: larkcli.Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: coordinator})
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := NewFsWithRunner(ctx, "test", "", configmap.Simple{"root_folder_token": "folder", "temp_dir": root}, runner)
	if err != nil {
		t.Fatal(err)
	}
	f := fsys.(*Fs)
	defer f.Shutdown(context.Background())
	o := f.newObject("file.txt", driveItem{Token: "file", Name: "file.txt", Type: "file"}, false)
	type openResult struct {
		reader io.ReadCloser
		err    error
	}
	opened := make(chan openResult, 1)
	go func() { rc, err := o.Open(ctx, &fs.RangeOption{Start: -1, End: 4}); opened <- openResult{rc, err} }()
	select {
	case <-downloadReady:
	case <-ctx.Done():
		t.Fatal("download did not reach barrier")
	}
	updated := make(chan error, 1)
	go func() {
		src := object.NewStaticObjectInfo("file.txt", time.Unix(1, 0), 3, true, nil, f)
		updated <- o.Update(context.WithValue(ctx, nativeOperationContextKey{}, "update"), strings.NewReader("new"), src)
	}()
	select {
	case <-coordinator.uploadDone:
	case <-ctx.Done():
		t.Fatal("upload did not complete transport")
	}
	// The old implementation's tiny unlock/relock gap cannot be forced here
	// without a production hook. These barriers prove actual overlapping I/O;
	// source-level locking must establish atomic materialization+reader binding.
	once.Do(func() { close(releaseDownload) })
	var first openResult
	select {
	case first = <-opened:
	case <-ctx.Done():
		t.Fatal("Open blocked")
	}
	if first.err != nil {
		t.Fatal(first.err)
	}
	defer first.reader.Close()
	select {
	case err = <-updated:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Update blocked")
	}
	b, err := io.ReadAll(first.reader)
	if err != nil || string(b) != "shot" {
		t.Fatalf("captured suffix=%q err=%v", b, err)
	}
	if o.Size() != 3 {
		t.Fatalf("updated size=%d", o.Size())
	}
	// Another Open must bind the new snapshot while the first reader stays valid.
	go func() { rc, err := o.Open(ctx); opened <- openResult{rc, err} }()
	select {
	case <-downloadReady:
	case <-ctx.Done():
		t.Fatal("new observation missing")
	}
	select {
	case next := <-opened:
		if next.err != nil {
			t.Fatal(next.err)
		}
		defer next.reader.Close()
		b, err = io.ReadAll(next.reader)
		if err != nil || string(b) != "new" {
			t.Fatalf("new snapshot=%q err=%v", b, err)
		}
	case <-ctx.Done():
		t.Fatal("new Open blocked")
	}
}
