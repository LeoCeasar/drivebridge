package manager

import (
	"context"
	"sync"
	"time"
)

// Graceful stop explicitly requests cloud completion, unlike preserving stop.
// New immutable jobs can appear while the paused mount finishes WAL archival,
// so keep bounded passes available until stop completes or its wait expires.
func startStopSyncDrain(parent context.Context, syncNow func(context.Context) error) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			_ = syncNow(ctx)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(cancel); <-done }
}
