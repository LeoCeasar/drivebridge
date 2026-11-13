package syncer

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

var errDispatchYield = errors.New("background pass yielded after verified operations")
var errEntryBusy = errors.New("entry has another active operation")

type quantumKey struct{}
type dispatchQuantum struct {
	operations int
	duration   time.Duration
}

type dispatchWork struct {
	cutoff          uint64
	epoch           uint64
	running, queued bool
	after           time.Time
}
type dispatchResult struct {
	id            model.EntryID
	cutoff, epoch uint64
	err           error
	before        model.JobID
	metadata      bool
}
type dispatchRequest struct {
	id       model.EntryID
	cutoff   uint64
	accepted chan struct{}
}

// One dispatcher owns its queue. Network goroutines report results; they never
// mutate admission state or start the next generation of their own file.
type backgroundDispatcher struct {
	e              *Engine
	ctx            context.Context
	cancel         context.CancelFunc
	next           time.Time
	epoch          uint64
	entries        map[model.EntryID]*dispatchWork
	queue          []model.EntryID
	active         int
	metadataActive bool
	metadataWanted bool
	results        chan dispatchResult
	requests       chan dispatchRequest
	done           chan struct{}
	wg             sync.WaitGroup
}

func newBackgroundDispatcher(ctx context.Context, e *Engine) *backgroundDispatcher {
	ctx, cancel := context.WithCancel(ctx)
	return &backgroundDispatcher{e: e, ctx: ctx, cancel: cancel, next: e.now().Add(BackgroundSyncInterval), entries: map[model.EntryID]*dispatchWork{}, results: make(chan dispatchResult, e.opt.Concurrency+1), requests: make(chan dispatchRequest), done: make(chan struct{})}
}
func (d *backgroundDispatcher) close() { d.cancel(); d.wg.Wait(); close(d.done) }
func (d *backgroundDispatcher) admit(id model.EntryID, cutoff uint64) {
	w := d.entries[id]
	if w == nil {
		w = &dispatchWork{}
		d.entries[id] = w
	}
	w.cutoff = max(w.cutoff, cutoff)
	w.epoch = d.epoch
	if !w.running && !w.queued {
		w.queued = true
		d.queue = append(d.queue, id)
	}
}
func (d *backgroundDispatcher) manual(ctx context.Context, id model.EntryID, cutoff uint64) error {
	r := dispatchRequest{id: id, cutoff: cutoff, accepted: make(chan struct{})}
	select {
	case d.requests <- r:
	case <-ctx.Done():
		return ctx.Err()
	case <-d.ctx.Done():
		return d.ctx.Err()
	}
	select {
	case <-r.accepted:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-d.ctx.Done():
		return d.ctx.Err()
	}
}
func (d *backgroundDispatcher) accept(r dispatchRequest) {
	d.epoch++
	d.admit(r.id, r.cutoff)
	close(r.accepted)
}
func (d *backgroundDispatcher) completed(r dispatchResult) {
	if r.metadata {
		d.metadataActive = false
		return
	}
	d.active--
	w := d.entries[r.id]
	if w == nil {
		return
	}
	w.running = false
	again := errors.Is(r.err, errDispatchYield) || errors.Is(r.err, errEntryBusy) || w.epoch > r.epoch
	if r.err == nil {
		again = again || d.hasAdmittedWork(r.id, w.cutoff, r.before)
	}
	if again && d.ctx.Err() == nil {
		if errors.Is(r.err, errEntryBusy) {
			w.after = time.Now().Add(time.Second)
		}
		w.queued = true
		d.queue = append(d.queue, r.id)
	} else {
		delete(d.entries, r.id)
	}
}
func (d *backgroundDispatcher) hasAdmittedWork(id model.EntryID, cutoff uint64, before model.JobID) bool {
	jobs, err := d.e.store.Pending(d.ctx)
	if err != nil {
		return false
	}
	for _, j := range jobs {
		if j.EntryID != id {
			continue
		}
		return j.ID != before && j.Generation <= cutoff && d.e.retryReady(j.ID) && (j.State == model.StatusQueued || j.State == model.StatusUploading || j.State == model.StatusMerging)
	}
	return false
}
func (d *backgroundDispatcher) tick() error {
	for {
		select {
		case r := <-d.results:
			d.completed(r)
		default:
			goto drained
		}
	}
drained:
	now := d.e.now()
	if !now.Before(d.next) {
		d.next = d.next.Add((now.Sub(d.next)/BackgroundSyncInterval + 1) * BackgroundSyncInterval)
		jobs, err := d.e.store.Pending(d.ctx)
		if err != nil {
			return err
		}
		pubs, err := d.e.store.PendingPublications(d.ctx)
		if err != nil {
			return err
		}
		cutoffs := map[model.EntryID]uint64{}
		for _, j := range jobs {
			cutoffs[j.EntryID] = max(cutoffs[j.EntryID], j.Generation)
		}
		for _, p := range pubs {
			cutoffs[p.EntryID] = max(cutoffs[p.EntryID], p.ExpectedGeneration)
		}
		ids := make([]model.EntryID, 0, len(cutoffs))
		for id := range cutoffs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		d.epoch++
		for _, id := range ids {
			d.admit(id, cutoffs[id])
		}
		d.metadataWanted = true
	}
	d.pump()
	return nil
}
func (d *backgroundDispatcher) pump() {
	if d.ctx.Err() != nil || d.e.backgroundShouldYield() {
		return
	}
	count := len(d.queue)
	for d.active < d.e.opt.Concurrency && len(d.queue) > 0 && count > 0 {
		count--
		id := d.queue[0]
		d.queue = d.queue[1:]
		w := d.entries[id]
		if time.Now().Before(w.after) {
			d.queue = append(d.queue, id)
			continue
		}
		w.queued = false
		w.running = true
		d.active++
		cutoff, epoch := w.cutoff, w.epoch
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			var before model.JobID
			if jobs, err := d.e.store.Pending(d.ctx); err == nil {
				for _, j := range jobs {
					if j.EntryID == id {
						before = j.ID
						break
					}
				}
			}
			err := d.pass(id, cutoff)
			d.results <- dispatchResult{id: id, cutoff: cutoff, epoch: epoch, err: err, before: before}
		}()
	}
	if d.metadataWanted && !d.metadataActive {
		d.metadataWanted = false
		d.metadataActive = true
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			ctx, cancel := context.WithTimeout(d.ctx, d.e.opt.BackgroundTimeout)
			defer cancel()
			err := d.e.syncNamespace(ctx)
			if ctx.Err() == nil && d.e.deps.RefreshDirectories != nil {
				err = errors.Join(err, d.e.deps.RefreshDirectories(ctx))
			}
			d.results <- dispatchResult{metadata: true, err: err}
		}()
	}
}
func (d *backgroundDispatcher) pass(id model.EntryID, cutoff uint64) error {
	en, err := d.e.store.Entry(d.ctx, id)
	if err != nil {
		return err
	}
	budget := d.e.opt.BackgroundTimeout
	if en.Kind == model.KindFile {
		budget = d.e.opt.TransferTimeout
	}
	ctx, cancel := context.WithTimeout(d.ctx, budget)
	defer cancel()
	ctx = context.WithValue(ctx, quantumKey{}, dispatchQuantum{d.e.opt.SliceOperations, d.e.opt.SliceDuration})
	err = d.e.syncEntry(ctx, id, &cutoff, true)
	if ctx.Err() == nil && d.e.deps.OnConflict != nil {
		conflicts, listErr := d.e.store.Conflicts(ctx)
		if listErr == nil {
			for _, c := range conflicts {
				if c.EntryID == id {
					_ = d.e.deps.OnConflict(ctx, c)
				}
			}
		}
	}
	return err
}
