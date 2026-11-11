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
