package desktop

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"strings"
)

func TestHeadlessInitializesPrivateRootsBeforeUIAndOwnsUntilQuit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("startup task query requires injected native Windows runner")
	}
	home := desktopTestHome(t)
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
	// A fresh desktop must claim the private state/cache roots before creating
	// State/ui, so the first management-page profile can be persisted.
	profile, err := m.AddProfile(ctx, "first account", "feishu")
	if err != nil {
		t.Fatalf("first desktop profile could not be saved: %v", err)
	}
	persisted, err := m.LoadConfig(ctx)
	if err != nil || len(persisted.Profiles) != 1 || persisted.Profiles[0].ID != profile.ID {
		t.Fatalf("first desktop profile not persisted: %+v, %v", persisted.Profiles, err)
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

func desktopTestHome(t *testing.T) string {
	t.Helper()
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

	return home
}

func TestHeadlessRefusesUnclaimedOccupiedStateBeforeUI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Known Folder needs injected path provider")
	}
	desktopTestHome(t)
	paths, err := config.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(paths.StateDir, "unrelated")
	if err := os.WriteFile(unrelated, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := manager.New(manager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = Run(ctx, Options{Manager: m, Headless: true})
	if err == nil || !strings.Contains(err.Error(), "refusing occupied private directory") {
		t.Fatalf("occupied state was accepted: %v", err)
	}
	for _, leaf := range []string{".drivebridge-owner", "ui"} {
		if _, err := os.Lstat(filepath.Join(paths.StateDir, leaf)); !os.IsNotExist(err) {
			t.Fatalf("rejected root changed: %s: %v", leaf, err)
		}
	}
	raw, err := os.ReadFile(unrelated)
	if err != nil || string(raw) != "preserve" {
		t.Fatalf("unrelated file changed: %q %v", raw, err)
	}
}

type desktopDiscoveryRunner struct {
	started, stopped chan struct{}
}

func (r *desktopDiscoveryRunner) RunDiscovery(ctx context.Context) {
	close(r.started)
	<-ctx.Done()
	close(r.stopped)
}
func TestDesktopOwnsDiscoveryGoroutineUntilCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &desktopDiscoveryRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	var operations sync.WaitGroup
	startDiscovery(ctx, runner, &operations)
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("discovery did not start")
	}
	cancel()
	joined := make(chan struct{})
	go func() { operations.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("desktop did not join discovery")
	}
	select {
	case <-runner.stopped:
	default:
		t.Fatal("discovery survived desktop ownership")
	}
}

func TestDesktopShutdownClosesOwnedResourcesBeforeNativeTermination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error)
	events := make(chan string, 8)
	operationsDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		<-operationsDone
		events <- "operations drained"
		finished <- nil
	}()
	shutdown := desktopShutdown(func() { events <- "app canceled"; cancel() }, finished,
		func() error { events <- "UI closed"; return nil },
		func() error { events <- "browser manager closed"; return nil },
		func() { events <- "singleton released" })
	terminated := make(chan struct{})
	go func() {
		// Model macOS's native exit: cleanup must finish before termination,
		// because termination does not unwind the caller's Go defers.
		if err := shutdown(); err != nil {
			t.Error(err)
		}
		events <- "native terminated"
		close(terminated)
	}()
	if got := <-events; got != "app canceled" {
		t.Fatal(got)
	}
	select {
	case <-terminated:
		t.Fatal("terminated with active operations")
	default:
	}
	close(operationsDone)
	select {
	case <-terminated:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not reach native termination")
	}
	for _, want := range []string{"operations drained", "UI closed", "browser manager closed", "singleton released", "native terminated"} {
		if got := <-events; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	// A returning native loop and a command-level cleanup can arrive later.
	// Neither may consume finished again or close the same resources twice.
	repeated := make(chan error, 1)
	go func() { repeated <- shutdown() }()
	select {
	case err := <-repeated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup consumed finished twice")
	}
	select {
	case event := <-events:
		t.Fatal("cleanup repeated", event)
	default:
	}
}

func TestConflictManagementURLPreservesPrivateBootstrap(t *testing.T) {
	got, err := managementViewURL("http://127.0.0.1:1234/?existing=value#bootstrap=private-token", "conflicts")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil || u.Query().Get("view") != "conflicts" || u.Query().Get("existing") != "value" || u.Fragment != "bootstrap=private-token" {
		t.Fatalf("conflicts URL lost bootstrap or target: %q (%v)", got, err)
	}
}
