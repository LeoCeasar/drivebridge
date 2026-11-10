package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerStartSharesLifecycleDomain(t *testing.T) {
	s, _, _, _, root := fixture(t)
	c, err := newCoordinatorAt(root)
	if err != nil {
		t.Fatal(err)
	}
	release, err := c.AcquireWorkerStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if unlock, err := s.lifecycleLease(context.Background()); !errors.Is(err, ErrBusy) {
		if unlock != nil {
			unlock()
		}
		t.Fatal("worker start escaped lifecycle lease", err)
	}
	release()
	exclusive, err := s.lifecycleLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if unlock, err := c.AcquireWorkerStart(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatal("start admitted during auth lifecycle", err)
	}
	exclusive()
}
func TestExclusiveLifecycleWaitsForWorkerStarts(t *testing.T) {
	c, err := newCoordinatorAt(privateTemp(t))
	if err != nil {
		t.Fatal(err)
	}
	release, err := c.AcquireWorkerStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if unlock, err := c.AcquireLifecycle(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatal(err)
	}
	release()
	unlock, err := c.AcquireLifecycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
