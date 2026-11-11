package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetadataIndexDoesNotBlockSyncAndDrainsOnExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	indexStarted, indexStopped, syncStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	run := metadataIndexedRun(func(ctx context.Context) error { close(syncStarted); <-ctx.Done(); return ctx.Err() }, func(ctx context.Context) error {
		close(indexStarted)
		<-ctx.Done()
		close(indexStopped)
		return ctx.Err()
	}, time.Hour)
	result := make(chan error, 1)
	go func() { result <- run(ctx) }()
	for _, ch := range []chan struct{}{indexStarted, syncStarted} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("index blocked the sync engine")
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
	select {
	case <-indexStopped:
	default:
		t.Fatal("index survived worker shutdown")
	}
}
func TestMetadataIndexFailureRetriesWithoutStoppingSync(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	retried := make(chan struct{})
	run := metadataIndexedRun(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func(ctx context.Context) error {
		if calls.Add(1) == 2 {
			close(retried)
		}
		return errors.New("offline")
	}, time.Millisecond)
	result := make(chan error, 1)
	go func() { result <- run(ctx) }()
	select {
	case <-retried:
	case err := <-result:
		t.Fatalf("metadata error stopped sync: %v", err)
	case <-time.After(time.Second):
		t.Fatal("no retry")
	}
	cancel()
	<-result
}

func TestMetadataIndexWakeContinuesBeforePeriodicInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wakes := make(chan struct{}, 1)
	started := make(chan struct{}, 2)
	run := metadataIndexedRun(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func(context.Context) error { started <- struct{}{}; return nil }, time.Hour, wakes)
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initial indexing missing")
	}
	wakes <- struct{}{}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("explicit rescan waited for periodic interval")
	}
	cancel()
	<-done
}
