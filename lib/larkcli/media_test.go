package larkcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var mediaPNG = []byte("\x89PNG\r\n\x1a\nfixture image data")

// A separate entry point keeps the managed subprocess fixture independent of
// other transports and exercises actual argv, cwd, files, cancellation and reap.
func TestMediaCLIProcess(t *testing.T) {
	scenario := os.Getenv("DB_MEDIA_SCENARIO")
	if scenario == "" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) < 2 || args[0] != "docs" || args[1] != "+media-download" || arg(args, "--token") != "imageToken" || arg(args, "--output") != "media" || arg(args, "--as") != "user" || !strings.Contains(strings.Join(args, " "), "--json") {
		os.Exit(91)
	}
	cwd, err := os.Getwd()
	info, statErr := os.Stat(cwd)
	// Windows reports synthetic directory permission bits; its directory access
	// is governed by ACLs rather than the Unix mode supplied to MkdirTemp.
	if err != nil || statErr != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) || os.Getenv("LARKSUITE_CLI_CONFIG_DIR") == "" {
		os.Exit(92)
	}
	check := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "media fixture setup:", err)
			os.Exit(93)
		}
	}
	body := mediaPNG
	saved := "media"
	size := any(len(body))
	result := ""
	switch scenario {
	case "text":
		body = []byte("<html>SECRET</html>")
		size = len(body)
	case "empty":
		body = nil
		size = 0
	case "mismatch":
		size = len(body) + 1
	case "oversize":
		size = 64*1024*1024 + 1
	case "quoted":
		size = fmt.Sprint(len(body))
	case "negative":
		size = -1
	case "large-file":
		size = 64*1024*1024 + 1
	case "absolute":
		saved = filepath.Join(cwd, "media")
	case "auto-png":
		saved = "media.png"
	case "auto-absolute-png":
		saved = filepath.Join(cwd, "media.png")
	case "auto-uppercase-png":
		saved = "media.PNG"
	case "auto-wrong-extension":
		saved = "media.jpg"
	case "auto-unknown-extension":
		saved = "media.exe"
	case "auto-sibling":
		saved = "other.png"
	case "auto-traversal":
		saved = "../media.png"
	case "auto-double-extension":
		saved = "media.exe.png"
	case "escape":
		saved = "../SECRET"
	case "different":
		saved = "other"
	case "failed":
		result = "failed"
	case "pending":
		result = "pending"
	case "success":
		result = "success"
	}
	if scenario == "symlink" {
		check(os.WriteFile("other", body, 0600))
		check(os.Symlink("other", "media"))
	} else if scenario == "directory" {
		check(os.Mkdir("media", 0700))
	} else if scenario != "missing" {
		check(os.WriteFile("media", body, 0600))
	}
	if strings.HasPrefix(scenario, "auto-") && scenario != "auto-traversal" {
		check(os.Rename("media", saved))
	}
	if scenario == "large-file" {
		check(os.Truncate("media", 64*1024*1024+1))
	}
	if scenario == "public-directory" {
		check(os.Chmod(cwd, 0755))
	}
	if scenario == "cancel" {
		time.Sleep(time.Minute)
	}
	identity := "user"
	if scenario == "identity" {
		identity = "tenant"
	}
	data := map[string]any{"saved_path": saved, "size_bytes": size, "content_type": "image/png"}
	if result != "" {
		data["result"] = result
	}
	if scenario == "no-size" {
		delete(data, "size_bytes")
	}
	if scenario == "null-result" {
		data["result"] = nil
	}
	if scenario == "null-size" {
		data["size_bytes"] = nil
	}
	if scenario == "null-path" {
		data["saved_path"] = nil
	}
	if scenario == "no-path" {
		delete(data, "saved_path")
	}
	envelope := map[string]any{"ok": true, "identity": identity, "data": data}
	if scenario == "error" {
		fmt.Fprint(os.Stderr, `{"ok":false,"identity":"user","error":{"type":"api","code":403,"message":"SECRET TOKEN"}}`)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(envelope)
	if scenario == "exit" {
		os.Exit(1)
	}
	os.Exit(0)
}

func mediaExecutable(t *testing.T, scenario string) string {
	t.Helper()
	bin, err := os.Executable()
	if runtime.GOOS == "windows" {
		return windowsFakeCLI(t, bin, "DB_MEDIA_SCENARIO", "TestMediaCLIProcess", scenario)
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "cli")
	script := "#!/bin/sh\nDB_MEDIA_SCENARIO='" + scenario + "' exec '" + strings.ReplaceAll(bin, "'", "'\\''") + "' -test.run=TestMediaCLIProcess -- \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}
func mediaDrive(t *testing.T, scenario string) (*Drive, string, *testCoordinator) {
	t.Helper()
	temp := t.TempDir()
	coord := new(testCoordinator)
	r, err := NewManaged(ManagedOptions{Executable: mediaExecutable(t, scenario), AuthRef: "ref", AuthRoot: t.TempDir(), Identity: Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: coord})
	if err != nil {
		t.Fatal(err)
	}
	return NewDrive(r, temp), temp, coord
}
func assertMediaClean(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files retained: %d, %v", len(entries), err)
	}
}
func TestOpenDocMediaPrivateRelativeDownloadAndCleanup(t *testing.T) {
	scenarios := []string{"normal", "absolute", "success", "auto-png", "auto-absolute-png", "auto-uppercase-png"}
	if runtime.GOOS == "windows" {
		// Directory Chmod does not alter Windows ACLs. Exercise the download
		// after it too, rather than treating synthetic POSIX bits as access.
		scenarios = append(scenarios, "public-directory")
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			d, temp, coord := mediaDrive(t, scenario)
			rc, err := d.OpenDocMedia(context.Background(), "imageToken")
			if err != nil {
				t.Fatal(err)
			}
			sized, ok := rc.(interface{ Stat() (os.FileInfo, error) })
			if !ok {
				t.Fatal("reader does not expose size")
			}
			info, err := sized.Stat()
			if err != nil || info.Size() != int64(len(mediaPNG)) {
				t.Fatal("incorrect reader size", err)
			}
			seeker, ok := rc.(io.Seeker)
			if !ok {
				t.Fatal("reader does not support ranges")
			}
			if _, err := seeker.Seek(8, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			tail, err := io.ReadAll(rc)
			if err != nil || !bytes.Equal(tail, mediaPNG[8:]) {
				t.Fatal("range bytes changed", err)
			}
			if _, err := seeker.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(rc)
			if err != nil || !bytes.Equal(body, mediaPNG) || coord.calls != 1 {
				t.Fatalf("download: bytes=%d, locks=%d, err=%v", len(body), coord.calls, err)
			}
			entries, _ := os.ReadDir(temp)
			if len(entries) != 1 {
				t.Fatal("lease missing")
			}
			if err := rc.Close(); err != nil {
				t.Fatal(err)
			}
			if err := rc.Close(); err != nil {
				t.Fatal("close not idempotent", err)
			}
			assertMediaClean(t, temp)
		})
	}
}
func TestOpenDocMediaRejectsIncompleteUnownedOrNonImageOutput(t *testing.T) {
	scenarios := []string{"text", "empty", "mismatch", "oversize", "large-file", "quoted", "negative", "escape", "different", "symlink", "directory", "missing", "failed", "pending", "null-result", "no-size", "null-size", "no-path", "null-path", "auto-wrong-extension", "auto-unknown-extension", "auto-sibling", "auto-traversal", "auto-double-extension", "identity", "exit", "error"}
	if runtime.GOOS != "windows" {
		// Only Unix directory permission bits can be made public by Chmod.
		scenarios = append(scenarios, "public-directory")
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			d, temp, _ := mediaDrive(t, scenario)
			rc, err := d.OpenDocMedia(context.Background(), "imageToken")
			if err == nil || rc != nil {
				if rc != nil {
					rc.Close()
				}
				t.Fatal("invalid download published")
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "imageToken") {
				t.Fatal("sensitive data leaked")
			}
			// A fixture launch/setup failure must not masquerade as validation of
			// malformed media or a provider error envelope.
			switch scenario {
			case "identity":
				if !errors.Is(err, ErrIdentity) {
					t.Fatal("identity lost", err)
				}
			case "text":
				if !errors.Is(err, ErrUnsupportedCapability) {
					t.Fatal("non-image rejection lost", err)
				}
			case "failed", "pending", "exit", "error":
				wantType := "operation"
				if scenario == "exit" {
					wantType = "process"
				} else if scenario == "error" {
					wantType = "api"
				}
				var cliErr *Error
				if !errors.As(err, &cliErr) || cliErr.Type != wantType {
					t.Fatalf("wrong CLI error: %#v", err)
				}
				if scenario == "error" && cliErr.Code != "403" {
					t.Fatalf("provider code lost: %#v", cliErr)
				}
			default:
				var cliErr *Error
				if errors.As(err, &cliErr) || !errors.Is(err, ErrProtocol) {
					t.Fatalf("wrong media validation error: %#v", err)
				}
			}
			assertMediaClean(t, temp)
		})
	}
}
func TestOpenDocMediaRejectsTokensBeforeDispatch(t *testing.T) {
	d, temp, coord := mediaDrive(t, "normal")
	for _, token := range []string{"", "https://example.invalid/image", "../image", "/image", `C:\image`, "a/b", "a\\b", "--json", "a?b", "a\x00b", "a b", strings.Repeat("a", 257)} {
		if rc, err := d.OpenDocMedia(context.Background(), token); err == nil || rc != nil {
			t.Fatalf("invalid token accepted")
		}
	}
	if coord.calls != 0 {
		t.Fatal("invalid token dispatched")
	}
	assertMediaClean(t, temp)
}
func TestOpenDocMediaCancellationCleansPartialAndReturnedLease(t *testing.T) {
	d, temp, _ := mediaDrive(t, "cancel")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	rc, err := d.OpenDocMedia(ctx, "imageToken")
	if !errors.Is(err, context.DeadlineExceeded) || rc != nil {
		t.Fatal("cancellation lost", err)
	}
	assertMediaClean(t, temp)
	d, temp, _ = mediaDrive(t, "normal")
	ctx, cancel = context.WithCancel(context.Background())
	rc, err = d.OpenDocMedia(ctx, "imageToken")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		entries, _ := os.ReadDir(temp)
		if len(entries) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled lease retained")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := rc.Read(make([]byte, 1)); err == nil {
		t.Fatal("canceled reader still readable")
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	assertMediaClean(t, temp)
}

func TestOpenDocMediaCancellationAfterPartialFileCreated(t *testing.T) {
	d, temp, _ := mediaDrive(t, "cancel")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		rc, err := d.OpenDocMedia(ctx, "imageToken")
		if rc != nil {
			_ = rc.Close()
		}
		done <- err
	}()
	defer cancel()
	// Synchronize on the actual download file so this cannot pass merely by
	// canceling before Windows has started the helper process.
	deadline := time.Now().Add(5 * time.Second)
	partialCreated := false
	for !partialCreated {
		entries, err := os.ReadDir(temp)
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		for _, entry := range entries {
			body, err := os.ReadFile(filepath.Join(temp, entry.Name(), "media"))
			if err == nil && bytes.Equal(body, mediaPNG) {
				partialCreated = true
				break
			}
		}
		if partialCreated {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("download ended before partial file was created: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("download did not create a partial file")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("partial download cancellation lost", err)
	}
	assertMediaClean(t, temp)
}
