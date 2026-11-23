package manager

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

func TestDirectoryIndexerScopedWakeAndDeadlineAreIndependentOfSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wake := make(chan struct{}, 1)
		var sweeps, requests atomic.Int32
		var due time.Time
		run := directoryIndexedRun(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func(context.Context) error { sweeps.Add(1); return nil }, func(context.Context) error {
			requests.Add(1)
			if !due.IsZero() && !time.Now().Before(due) {
				due = time.Time{}
			}
			return nil
		}, func() (time.Duration, bool) {
			if due.IsZero() {
				return 0, false
			}
			return time.Until(due), true
		}, time.Hour, wake)
		done := make(chan error, 1)
		go func() { done <- run(ctx) }()
		synctest.Wait()
		if sweeps.Load() != 1 {
			t.Fatal("initial ordinary scan missing")
		}
		due = time.Now().Add(45 * time.Second)
		wake <- struct{}{}
		synctest.Wait()
		if requests.Load() != 1 || sweeps.Load() != 1 {
			t.Fatal("scoped wake read unrelated directories", requests.Load(), sweeps.Load())
		}
		time.Sleep(45 * time.Second)
		synctest.Wait()
		if requests.Load() != 2 || sweeps.Load() != 1 {
			t.Fatal("delayed request waited for hour-long sweep", requests.Load(), sweeps.Load())
		}
		cancel()
		<-done
	})
}
