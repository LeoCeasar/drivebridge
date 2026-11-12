package syncer

import (
	"sync"
	"time"
)

// Interactive conflict work (opening a conflict, accepting a merge) is a
// user-visible foreground request. Every Feishu read of one account goes through
// the same exclusive credential lock, one CLI process at a time, so a background
// poll or upload for a different entry that starts in the middle of a batch
// queues ahead of the next file's reads. After the first file of a batch was
// accepted, its own write-back became due while the second file was still
// verifying, which is the "second item stalls" report.
//
// Background loops therefore yield while a foreground request is running and for
// a short grace window after it, which covers the gap between two files of one
// batch. Yielding only delays background work: durable jobs stay queued, and the
// deferral is capped so a hung foreground request can never starve them.
const (
	defaultForegroundGrace = 3 * time.Second
	maxBackgroundDeferral  = 30 * time.Second
)

type foregroundGate struct {
	mu        sync.Mutex
	active    int
	until     time.Time // end of the grace window after the latest foreground request
	deferring time.Time // start of the current uninterrupted deferral streak
}

func (e *Engine) foregroundGrace() time.Duration {
	if e.opt.ForegroundGrace == 0 {
		return defaultForegroundGrace
	}
	return e.opt.ForegroundGrace
}

// beginForeground marks the start of an interactive request. The returned
// function must be called exactly once when it finishes.
func (e *Engine) beginForeground() func() {
	g := &e.foreground
	g.mu.Lock()
	g.active++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			g.until = e.now().Add(e.foregroundGrace())
			g.mu.Unlock()
		})
	}
}

// backgroundShouldYield reports whether a background cycle should skip its
// cloud work now. It is cheap and never blocks.
func (e *Engine) backgroundShouldYield() bool {
	if e.opt.ForegroundGrace < 0 {
		return false // gate disabled: legacy behavior
	}
	g := &e.foreground
	now := e.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == 0 && !now.Before(g.until) {
		g.deferring = time.Time{}
		return false
	}
	if g.deferring.IsZero() {
		g.deferring = now
	}
	return now.Sub(g.deferring) < maxBackgroundDeferral
}
