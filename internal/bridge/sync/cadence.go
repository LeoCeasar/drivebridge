package syncer

import (
	"context"
	"sync"
	"time"
)

// BackgroundSyncInterval is a fixed cloud cycle, independent of save activity.
// WAL acceptance and local history archival do not wait for this interval.
const BackgroundSyncInterval = 5 * time.Minute

type backgroundSchedule struct {
	mu      sync.Mutex
	next    time.Time
	running bool
}

func (e *Engine) backgroundCycle(ctx context.Context) error {
	s := &e.schedule
	now := e.now()
	s.mu.Lock()
	if s.next.IsZero() {
		s.next = now.Add(BackgroundSyncInterval)
		s.mu.Unlock()
		return nil
	}
	if now.Before(s.next) || s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = true
	s.next = s.next.Add((now.Sub(s.next)/BackgroundSyncInterval + 1) * BackgroundSyncInterval)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		// Skip boundaries missed during a long upload; never immediately replay a
		// backlog of cycles against edits made while that upload was in flight.
		now := e.now()
		if !now.Before(s.next) {
			s.next = s.next.Add((now.Sub(s.next)/BackgroundSyncInterval + 1) * BackgroundSyncInterval)
		}
		s.running = false
		s.mu.Unlock()
	}()
	return e.cycleWithPolicy(ctx, true)
}

// SyncNow is an explicit cloud drain for manual sync and graceful stop. It
// bypasses the five-minute schedule and the old per-save write-back debounce.
func (e *Engine) SyncNow(ctx context.Context) error { return e.cycleWithPolicy(ctx, true) }
