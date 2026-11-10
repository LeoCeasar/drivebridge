package desktop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

type fixtureBackend struct {
	mu       sync.Mutex
	mounts   []Mount
	status   map[model.MountID]model.MountStatus
	starts   int
	startErr error
	started  chan struct{}
}

func (b *fixtureBackend) Mounts(context.Context) ([]Mount, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Mount(nil), b.mounts...), nil
}
func (b *fixtureBackend) Status(_ context.Context, id model.MountID) (model.MountStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status[id], nil
}
func (b *fixtureBackend) Start(_ context.Context, _ model.MountID, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	if b.started != nil {
		b.started <- struct{}{}
	}
	return b.startErr
}
func (b *fixtureBackend) Stop(context.Context, model.MountID) error       { return nil }
func (b *fixtureBackend) Retry(context.Context, model.MountID) error      { return nil }
func (b *fixtureBackend) OpenManagement(context.Context) error            { return nil }
func (b *fixtureBackend) OpenFolder(context.Context, model.MountID) error { return nil }
func (b *fixtureBackend) SetAutostart(context.Context, bool) error        { return nil }
func (b *fixtureBackend) Autostart(context.Context) (bool, error)         { return false, nil }

func TestRecoveryRespectsDesireAndBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &fixtureBackend{mounts: []Mount{{ID: "a", Desired: true}, {ID: "b", Desired: false}}, status: map[model.MountID]model.MountStatus{}, startErr: errors.New("offline"), started: make(chan struct{}, 4)}
	now := time.Now()
	c := NewController(ctx, b, ControllerOptions{Now: func() time.Time { return now }, ErrorCode: func(error) string { return "network" }})
	c.Poll(ctx)
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	c.Wait()
	c.Poll(ctx)
	c.Wait()
	b.mu.Lock()
	n := b.starts
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("backoff ignored: %d", n)
	}
	now = now.Add(3 * time.Second)
	c.Poll(ctx)
	c.Wait()
	b.mu.Lock()
	n = b.starts
	b.mounts[0].Desired = false
	b.mu.Unlock()
	if n != 2 {
		t.Fatalf("retry missing: %d", n)
	}
	now = now.Add(time.Hour)
	c.Poll(ctx)
	c.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.starts != 2 {
		t.Fatal("stopped mount restarted")
	}
}

func TestRecoveryBoundedAndAuthNeverAutoRetried(t *testing.T) {
	for _, code := range []string{"network", "unauthorized"} {
		t.Run(code, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &fixtureBackend{mounts: []Mount{{ID: "a", Desired: true}}, status: map[model.MountID]model.MountStatus{}, startErr: errors.New("failure")}
			now := time.Now()
			c := NewController(ctx, b, ControllerOptions{Now: func() time.Time { return now }, ErrorCode: func(error) string { return code }})
			for range 10 {
				c.Poll(ctx)
				c.Wait()
				now = now.Add(time.Hour)
			}
			b.mu.Lock()
			n := b.starts
			b.mu.Unlock()
			want := 5
			if code == "unauthorized" {
				want = 1
			}
			if n != want {
				t.Fatalf("starts=%d want %d", n, want)
			}
		})
	}
}
