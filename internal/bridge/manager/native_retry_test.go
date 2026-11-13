package manager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestRetryPendingEntriesIsolatesSlowFileAndJoinsFailures(t *testing.T) {
	held, entered, healthy := make(chan struct{}), make(chan struct{}), make(chan struct{})
	want := errors.New("blocked file")
	jobs := []model.Job{{EntryID: "slow"}, {EntryID: "healthy"}, {EntryID: "blocked"}, {EntryID: "healthy"}}
	var mu sync.Mutex
	calls := map[model.EntryID]int{}
	done := make(chan error, 1)
	go func() {
		done <- retryPendingEntries(context.Background(), jobs, func(ctx context.Context, id model.EntryID) error {
			mu.Lock()
			calls[id]++
			mu.Unlock()
			switch id {
			case "slow":
				close(entered)
				<-held
			case "healthy":
				close(healthy)
			case "blocked":
				return want
			}
			return nil
		})
	}()
	var release sync.Once
	defer release.Do(func() { close(held) })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow retry did not start")
	}
	select {
	case <-healthy:
	case <-time.After(time.Second):
		t.Fatal("healthy retry blocked behind another file")
	}
	// Let every worker drain before inspecting shared outcomes.
	release.Do(func() { close(held) })
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("failure lost: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["healthy"] != 1 {
		t.Fatalf("same entry retried %d times", calls["healthy"])
	}
}

func TestRetryPendingEntriesBoundsConcurrentRequests(t *testing.T) {
	held, started := make(chan struct{}), make(chan struct{}, 10)
	jobs := make([]model.Job, 10)
	for i := range jobs {
		jobs[i].EntryID = model.EntryID(string(rune('a' + i)))
	}
	done := make(chan error, 1)
	go func() {
		done <- retryPendingEntries(context.Background(), jobs, func(context.Context, model.EntryID) error { started <- struct{}{}; <-held; return nil })
	}()
	var release sync.Once
	defer release.Do(func() { close(held) })
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("independent retries serialized")
		}
	}
	select {
	case <-started:
		t.Fatal("more than three requests active")
	case <-time.After(25 * time.Millisecond):
	}
	release.Do(func() { close(held) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
