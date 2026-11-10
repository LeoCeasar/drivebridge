package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
)

func TestHeadlessOwnsUIUntilAuthenticatedQuit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("startup task query requires injected native Windows runner")
	}
	// Every state/auth/cache path is redirected before constructing the manager.
	home, canonicalErr := filepath.EvalSymlinks(t.TempDir())
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, filepath.Join(home, key))
		if err := os.MkdirAll(os.Getenv(key), 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(home, "personal.json")
	m, err := manager.New(manager.Options{ConfigFile: file})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan string, 2)
	done := make(chan error, 1)
	exited := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("desktop leaked after test")
		}
	})
	go func() {
		defer close(exited)
		done <- Run(ctx, Options{Manager: m, Executable: filepath.Join(home, "drivebridge-desktop"), Headless: true, Open: func(_ context.Context, raw string) error { opened <- raw; return nil }})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		running, e := m.DesktopRunning(ctx)
		if e == nil && running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready: %v", e)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = m.DesktopOpen(ctx); err != nil {
		t.Fatal(err)
	}
	var address string
	select {
	case address = <-opened:
	case <-time.After(time.Second):
		t.Fatal("management not opened")
	}
	u, err := url.Parse(address)
	if err != nil || u.Hostname() != "127.0.0.1" || u.Fragment == "" {
		t.Fatal("private bootstrap missing")
	}
	u.Fragment = ""
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("management host unavailable")
	}
	// A duplicate must release only its own resources, keeping the first UI alive.
	other, err := manager.New(manager.Options{ConfigFile: file})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	duplicate := Run(ctx, Options{Manager: other, Executable: filepath.Join(home, "drivebridge-desktop"), Headless: true})
	if !errors.Is(duplicate, manager.ErrDesktopRunning) {
		t.Fatalf("duplicate: %v", duplicate)
	}
	response, err = client.Get(u.String())
	if err != nil {
		t.Fatal("duplicate closed original UI:", err)
	}
	response.Body.Close()
	if err = m.DesktopQuit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("desktop did not close")
	}
	if running, err := m.DesktopRunning(ctx); err != nil || running {
		t.Fatalf("ownership retained: %v %v", running, err)
	}
	if response, err = client.Get(u.String()); err == nil {
		response.Body.Close()
		t.Fatal("owned UI host survived quit")
	}
}

func TestAppBundleResolvesPairedWorker(t *testing.T) {
	root, canonicalErr := filepath.EvalSymlinks(t.TempDir())
	if canonicalErr != nil {
		t.Fatal(canonicalErr)
	}
	binary := filepath.Join(root, "DriveBridge.app", "Contents", "MacOS", "drivebridge-desktop")
	worker := filepath.Join(root, "bin", "drivebridge")
	for _, p := range []string{binary, worker} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("synthetic"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	got, app := Executables(binary)
	if got != worker || app != binary {
		t.Fatalf("%s %s", got, app)
	}
	got, app = Executables(worker)
	if got != worker || app != binary {
		t.Fatalf("%s %s", got, app)
	}
}
