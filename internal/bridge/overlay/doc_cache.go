package overlay

import (
	"context"
	"errors"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/doccache"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

const docMediaQueueLimit = 32
const docMediaStateLimit = 256
const docMediaRetryDelay = time.Minute

type docMediaFlight struct {
	done chan struct{}
	err  error
}

type docMediaState struct {
	queued  bool
	ready   bool
	retryAt time.Time
}

// ensureDocImages upgrades only reconstructible media cache. The accepted
// snapshot, draft, baseline and journal are immutable, even for a dirty document.
// Only callers for the same snapshot wait for each other.
func (f *Fs) ensureDocImages(ctx context.Context, en model.Entry) error {
	if en.Kind != model.KindDocx || en.RemoteCheckpoint == "" {
		return nil
	}
	id := en.RemoteCheckpoint
	f.docMediaMu.Lock()
	if flight := f.docMediaFlights[id]; flight != nil {
		f.docMediaMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-flight.done:
			return flight.err
		}
	}
	if f.docMediaFlights == nil {
		f.docMediaFlights = make(map[model.SnapshotID]*docMediaFlight)
	}
	flight := &docMediaFlight{done: make(chan struct{})}
	f.docMediaFlights[id] = flight
	f.docMediaMu.Unlock()

	sn, err := f.store.Snapshot(ctx, id)
	if err == nil {
		err = doccache.Ensure(ctx, f.store, f.source, sn)
	}
	f.docMediaMu.Lock()
	flight.err = err
	delete(f.docMediaFlights, id)
	if f.docMediaStates == nil {
		f.docMediaStates = make(map[model.SnapshotID]docMediaState)
	}
	state := f.docMediaStates[id]
	state.ready = err == nil
	if err != nil {
		state.retryAt = time.Now().Add(docMediaRetryDelay)
	}
	f.trimDocMediaStates()
	f.docMediaStates[id] = state
	close(flight.done)
	f.docMediaMu.Unlock()
	return err
}

// trimDocMediaStates bounds the opportunistic ready/retry memo. Pending work
// stays deduplicated even while older completed snapshots leave the memo.
func (f *Fs) trimDocMediaStates() {
	if len(f.docMediaStates) < docMediaStateLimit {
		return
	}
	for id, state := range f.docMediaStates {
		if !state.queued && f.docMediaFlights[id] == nil {
			delete(f.docMediaStates, id)
			return
		}
	}
}

func (f *Fs) queueDocImages(en model.Entry) {
	if en.Kind != model.KindDocx || en.RemoteCheckpoint == "" {
		return
	}
	f.docMediaMu.Lock()
	defer f.docMediaMu.Unlock()
	state := f.docMediaStates[en.RemoteCheckpoint]
	if f.docMediaClosed || state.ready || state.queued || time.Now().Before(state.retryAt) {
		return
	}
	if f.docMediaQueue == nil {
		workerCtx, cancel := context.WithCancel(context.Background())
		f.docMediaCancel = cancel
		f.docMediaQueue = make(chan model.Entry, docMediaQueueLimit)
		f.docMediaDone = make(chan struct{})
		go f.backfillDocImages(workerCtx)
	}
	select {
	case f.docMediaQueue <- en:
		if f.docMediaStates == nil {
			f.docMediaStates = make(map[model.SnapshotID]docMediaState)
		}
		state.queued = true
		f.trimDocMediaStates()
		f.docMediaStates[en.RemoteCheckpoint] = state
	default:
		// A later local read may retry; never block a cached text open.
	}
}

func (f *Fs) backfillDocImages(ctx context.Context) {
	defer close(f.docMediaDone)
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case en := <-f.docMediaQueue:
			jobCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := f.ensureDocImages(jobCtx, en)
			cancel()
			f.docMediaMu.Lock()
			state := f.docMediaStates[en.RemoteCheckpoint]
			state.queued = false
			f.docMediaStates[en.RemoteCheckpoint] = state
			f.docMediaMu.Unlock()
			if err != nil && ctx.Err() == nil {
				fs.Errorf(f, "Cache images for %q: %v", en.Remote, err)
			}
		}
	}
}

// Shutdown cancels and joins the lazy bounded media backfill worker before its
// durable store is closed. Repeated calls are safe.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.docMediaMu.Lock()
	f.docMediaClosed = true
	if f.docMediaCancel != nil {
		f.docMediaCancel()
	}
	done := f.docMediaDone
	f.docMediaMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PrepareManagedRead runs after VFS path normalization and admission, including
// warm VFS-cache hits. Accepted text reads only local metadata; legacy missing
// media is repaired opportunistically without changing text availability.
func (f *Fs) PrepareManagedRead(ctx context.Context, remote string) error {
	en, err := f.store.EntryByRemote(ctx, remote)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	f.queueDocImages(en)
	return nil
}
