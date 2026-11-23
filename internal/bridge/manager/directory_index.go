package manager

import (
	"context"
	"time"
)

// Directory metadata has its own worker so slow listings cannot delay durable
// uploads. The indexer never opens document bodies and is drained before exit.
func metadataIndexedRun(run, index func(context.Context) error, interval time.Duration, wakes ...<-chan struct{}) func(context.Context) error {
	var wake <-chan struct{}
	if len(wakes) > 0 {
		wake = wakes[0]
	}
	if interval <= 0 {
		interval = time.Minute
	}
	return func(ctx context.Context) error {
		if index == nil {
			return run(ctx)
		}
		life, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				if life.Err() != nil {
					return
				}
				_ = index(life)
				timer := time.NewTimer(interval)
				select {
				case <-life.Done():
					timer.Stop()
					return
				case <-timer.C:
				case <-wake:
					timer.Stop()
				}
			}
		}()
		err := run(life)
		cancel()
		<-done
		return err
	}
}

// Directory requests share the sweep worker but have their own due timer. A
// scoped wake must not start a new sweep of unrelated folders.
func directoryIndexedRun(run, sweep, requested func(context.Context) error, next func() (time.Duration, bool), interval time.Duration, wake <-chan struct{}) func(context.Context) error {
	if interval <= 0 {
		interval = time.Minute
	}
	return func(ctx context.Context) error {
		life, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if life.Err() != nil {
				return
			}
			_ = sweep(life)
			periodic := time.NewTimer(interval)
			defer periodic.Stop()
			for {
				if life.Err() != nil {
					return
				}
				// A stream of scoped wakes cannot starve an ordinary scan which is due.
				select {
				case <-periodic.C:
					_ = sweep(life)
					periodic.Reset(interval)
					continue
				default:
				}
				var delayed *time.Timer
				var ready <-chan time.Time
				if delay, ok := next(); ok {
					if delay < 0 {
						delay = 0
					}
					delayed = time.NewTimer(delay)
					ready = delayed.C
				}
				stopped := false
				select {
				case <-life.Done():
					stopped = true
				case <-periodic.C:
					_ = sweep(life)
					periodic.Reset(interval)
				case <-wake:
					_ = requested(life)
				case <-ready:
					_ = requested(life)
				}
				if delayed != nil {
					delayed.Stop()
				}
				if stopped {
					return
				}
			}
		}()
		err := run(life)
		cancel()
		<-done
		return err
	}
}
