package syncer

import (
	"context"
	"errors"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

type retryState struct {
	Attempts int
	Due      time.Time
}

func (e *Engine) now() time.Time {
	if e.opt.Now != nil {
		return e.opt.Now()
	}
	return time.Now()
}
func (e *Engine) deferRetry(id model.JobID, err error) {
	code := classify(err)
	if code != "network_or_protocol" && code != "rate_limited" && code != "canceled" {
		return
	}
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	state := e.retries[id]
	state.Attempts++
	delay := 2 * time.Second
	for i := 1; i < state.Attempts && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	var api *larkcli.Error
	if errors.As(err, &api) && api.RetryAfter > delay {
		delay = api.RetryAfter
	}
	state.Due = e.now().Add(delay)
	e.retries[id] = state
}
func (e *Engine) retryReady(id model.JobID) bool {
	e.retryMu.Lock()
	defer e.retryMu.Unlock()
	state, exists := e.retries[id]
	return !exists || !e.now().Before(state.Due)
}
func transient(code string) bool {
	return code == "network_or_protocol" || code == "rate_limited" || code == "canceled"
}

// Retry reconciles unknown work; it never changes an unknown mutation to queued.
func (e *Engine) Retry(ctx context.Context, id model.EntryID) error {
	if err := e.check(ctx); err != nil {
		return err
	}
	jobs, err := e.store.Pending(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.EntryID == id {
			if j.State == model.StatusBlocked || j.State == model.StatusPaused {
				if err = e.store.ResumeJob(ctx, j.ID); err != nil {
					return err
				}
			}
			e.retryMu.Lock()
			delete(e.retries, j.ID)
			e.retryMu.Unlock()
			break
		}
	}
	return e.syncEntry(ctx, id, nil, true)
}
