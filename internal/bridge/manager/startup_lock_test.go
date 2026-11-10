package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestWorkerStartupWaitsForShortOfflineStatusOwnership(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Resume has an existing Store, so an ordinary offline Status reads it
	// while holding this exact runtime ownership lock.
	s, err := store.Open(spec.Paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindScope(context.Background(), ownershipFor(m.ConfigFile(), spec).Scope); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	readerLock, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if err = readerLock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer readerLock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.RunWorker(ctx, id) }()
	select {
	case err = <-done:
		t.Fatalf("short offline reader defeated worker startup: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err = readerLock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err = m.waitEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	for {
		var status model.MountStatus
		if err = m.call(ctx, id, "/status", nil, &status); err == nil && status.Running {
			break
		}
		select {
		case err = <-done:
			t.Fatal("worker exited before ready", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWorkerStartupDoesNotTakeLiveWorkerOwnership(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	original := m.localWorker(id)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.RunWorker(ctx, id); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	if m.localWorker(id) != original {
		t.Fatal("live worker identity was replaced")
	}
	status, err := m.Status(context.Background(), id)
	if err != nil || !status.Running {
		t.Fatal(status, err)
	}
}

func TestWorkerStartupClaimHonorsCancellation(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	lock, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if err = lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = m.RunWorker(ctx, id)
	if !errors.Is(err, ErrPending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claim did not preserve cancellation: %v", err)
	}
	if m.localWorker(id) != nil {
		t.Fatal("busy worker was registered")
	}
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(spec.Paths.Root, "ipc.json")); !os.IsNotExist(err) {
		t.Fatal("ownership failure published an endpoint", err)
	}
}

func TestStartupStatusSkipsOfflineStoreAndRequiresAuthenticatedIPC(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(spec.Paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if err = lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if _, err = m.Status(context.Background(), id); !errors.Is(err, ErrPending) {
		t.Fatal("ordinary offline inspection no longer excludes runtime ownership", err)
	}
	if err = atomicJSON(filepath.Join(spec.Paths.Root, "last-state.json"), map[string]string{"last_error": "driver_unavailable"}); err != nil {
		t.Fatal(err)
	}
	status, err := m.startupStatus(context.Background(), spec)
	if err != nil || status.Running || status.LocalSaved || status.CloudState != "starting" || status.LastError != "driver_unavailable" {
		t.Fatalf("startup should use only IPC/error hints, not the locked Store: %+v %v", status, err)
	}
	if err = os.WriteFile(filepath.Join(spec.Paths.Root, "ipc.json"), []byte(`{"ready":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	status, err = m.startupStatus(context.Background(), spec)
	if !errors.Is(err, ErrInstanceChanged) || status.Running {
		t.Fatal("unbound readiness descriptor accepted", status, err)
	}
}
