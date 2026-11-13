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
	if !transient(code) {
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
	return code == "request_timeout" || code == "network_or_protocol" || code == "rate_limited" || code == "canceled" || code == "remote_drift"
}

// Retry reconciles unknown work; it never changes an unknown mutation to queued.
func (e *Engine) Retry(ctx context.Context, id model.EntryID) error {
	e.dispatchMu.RLock()
	dispatcher := e.dispatcher
	e.dispatchMu.RUnlock()
	// Admission is local only. The worker still verifies identity before every
	// pass and the transport guards every mutation, using its bounded context.
	if dispatcher == nil {
		if err := e.check(ctx); err != nil {
			return err
		}
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
	if dispatcher != nil {
		en, err := e.store.Entry(ctx, id)
		if err != nil {
			return err
		}
		return dispatcher.manual(ctx, id, en.Generation)
	}
	return e.syncEntry(ctx, id, nil, true)
}
