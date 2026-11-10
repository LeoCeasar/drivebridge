package manager

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesktopReservedWithoutDescriptorIsNotReady(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	lock, err := privateLock(m.desktopRunLock())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	defer lock.Unlock()
	if running, err := m.DesktopRunning(context.Background()); err != nil || running {
		t.Fatalf("reserved or retiring desktop must be not ready: %v %v", running, err)
	}
}

// Singleton ownership must cover all configurations in the OS-user domain and
// last through actual UI teardown, rather than ending at the quit request.
func TestDesktopSingletonAuthenticatesAndKeepsLockUntilRelease(t *testing.T) {
	ctx := context.Background()
	m, _, _, _ := fixture(t, nil)
	var opens atomic.Int32
	quit := make(chan struct{}, 1)
	release, err := m.DesktopRegister(ctx, func(context.Context) error { opens.Add(1); return nil }, func() { quit <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if running, err := m.DesktopRunning(ctx); err != nil || !running {
		t.Fatalf("missing desktop: %v %v", running, err)
	}
	if _, err := m.DesktopRegister(ctx, nil, nil); !errors.Is(err, ErrDesktopRunning) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := m.DesktopOpen(ctx); err != nil || opens.Load() != 1 {
		t.Fatalf("open: %d %v", opens.Load(), err)
	}
	ep, _, err := m.readDesktopEndpoint(true)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", ep.Address+"/quit", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer forged")
	req.Header.Set("X-DriveBridge-Instance", ep.InstanceID)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged token: %d", res.StatusCode)
	}
	select {
	case <-quit:
		t.Fatal("forged request quit desktop")
	default:
	}
	otherOptions := m.options
	otherOptions.ConfigFile = filepath.Join(filepath.Dir(m.ConfigFile()), "other.json")
	other, err := newManager(otherOptions, dependencies{domain: m.domain, startLease: m.startLease, lifecycleLease: m.lifecycleLease})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = other.DesktopRunning(ctx); !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("cross-config reused: %v", err)
	}
	if err = other.DesktopOpen(ctx); !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("cross-config open: %v", err)
	}
	if err = m.DesktopQuit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("quit not delivered")
	}
	if _, err := m.DesktopRegister(ctx, nil, nil); !errors.Is(err, ErrDesktopRunning) {
		t.Fatalf("lock released before UI teardown: %v", err)
	}
	release()
	if running, err := m.DesktopRunning(ctx); err != nil || running {
		t.Fatalf("desktop stayed live: %v %v", running, err)
	}
}

func TestInstallationLeaseWaitsForDesktopReleaseAcrossConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, id, _, _ := fixture(t, nil)
	installationGates(m)
	_, offline := offlineInstallationStore(t, m, id)
	if err := offline.Close(); err != nil {
		t.Fatal(err)
	}
	other, _, _, _ := fixture(t, nil)
	other.domain = m.domain
	installationGates(other)
	quit := make(chan struct{}, 1)
	release, err := other.DesktopRegister(ctx, nil, func() { quit <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.WithInstallationLease(ctx, func() error { close(entered); return nil }) }()
	select {
	case <-quit:
	case <-ctx.Done():
		t.Fatal("install did not request desktop quit")
	}
	select {
	case <-entered:
		t.Fatal("replacement before UI release")
	default:
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("replacement not called")
	}
}

func TestDesktopConcurrentRegistrationHasOneOwner(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	const callers = 8
	start := make(chan struct{})
	type result struct {
		release func()
		err     error
	}
	results := make(chan result, callers)
	for range callers {
		go func() {
			<-start
			release, err := m.DesktopRegister(context.Background(), nil, nil)
			results <- result{release, err}
		}()
	}
	close(start)
	owners := 0
	for range callers {
		got := <-results
		if got.err == nil {
			owners++
			defer got.release()
		} else if !errors.Is(got.err, ErrDesktopRunning) {
			t.Errorf("duplicate registration: %v", got.err)
		}
	}
	if owners != 1 {
		t.Fatalf("singleton owners: %d", owners)
	}
}
