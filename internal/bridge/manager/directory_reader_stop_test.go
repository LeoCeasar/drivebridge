package manager

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStopStillWaitsForFileReaders(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.readers = 1
	runtime.mu.Unlock()
	status, err := m.Status(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || !status.LocalSaved || status.VFSPending != 0 || status.Queued != 0 || status.Inflight != 0 || status.Conflicts != 0 {
		t.Fatalf("not clean: %+v", status)
	}
	t.Logf("before stop: running=%v local_saved=%v cloud=%s vfs_pending=%d queued=%d inflight=%d conflicts=%d", status.Running, status.LocalSaved, status.CloudState, status.VFSPending, status.Queued, status.Inflight, status.Conflicts)
	if err := m.Stop(context.Background(), id, 25*time.Millisecond); !errors.Is(err, ErrPending) {
		t.Fatalf("reader did not block stop: %v", err)
	}
	runtime.mu.Lock()
	if runtime.unmounted {
		t.Fatal("stop attempted unmount before reader release")
	}
	runtime.readers = 0
	runtime.mu.Unlock()
	if err := m.Stop(context.Background(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	t.Log("same clean worker stops successfully after reader count becomes zero")
}

func TestStopWithOnlyDirectoryReadersUsesNormalUnmount(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.directoryReaders = 1
	runtime.mu.Unlock()
	if err := m.Stop(context.Background(), id, 25*time.Millisecond); err != nil {
		t.Fatalf("directory enumeration prevented clean stop: %v", err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.unmounted {
		t.Fatal("stop succeeded without invoking normal unmount")
	}
}

func TestStopWithDirectoryReadersRequiresSuccessfulUnmount(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	want := errors.New("normal OS unmount rejected")
	runtime.mu.Lock()
	runtime.directoryReaders = 1
	runtime.unmountErr = want
	runtime.mu.Unlock()
	worker := m.localWorker(id)
	if err := worker.stop(context.Background(), 25*time.Millisecond); !errors.Is(err, want) {
		t.Fatalf("unmount failure hidden: %v", err)
	}
	select {
	case <-worker.stopped:
		t.Fatal("reported stopped after unsuccessful unmount")
	default:
	}
}
