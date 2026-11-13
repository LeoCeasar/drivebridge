// Package syncer coordinates durable local jobs and best-effort cloud readback.
package syncer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type Publisher interface {
	Publish(context.Context, model.PublishCandidate) error
}
type PublisherFunc func(context.Context, model.PublishCandidate) error

func (f PublisherFunc) Publish(ctx context.Context, c model.PublishCandidate) error { return f(ctx, c) }

type Dependencies struct {
	DeletionCommit     func(context.Context, model.Entry, func() (bool, error)) error
	CheckIdentity      func(context.Context) error
	OnConflict         func(context.Context, model.Conflict) error
	Store              *store.Store
	Cloud              *cloud.Client
	Source             Source
	Publisher          Publisher
	LocalOnly          func(string) bool
	ShouldSync         func(context.Context, model.EntryID) (bool, error)
	ShouldSyncPath     func(context.Context, string) (bool, error)
	ShouldRefresh      func(context.Context, model.EntryID) (bool, error)
	ActiveEntries      func() []model.EntryID
	RefreshDirectories func(context.Context) error
}
type Options struct {
	BackgroundTimeout, TransferTimeout, InteractiveTimeout time.Duration
	SliceOperations                                        int
	SliceDuration                                          time.Duration
	WriteBack                                              time.Duration
	EmptyFileUploadVerified                                bool // trusted capability evidence, not a config override
	Now                                                    func() time.Time
	Concurrency                                            int
	PollInterval, DirectoryInterval                        time.Duration
	// ForegroundGrace is how long background cloud work yields after an
	// interactive conflict request. Zero selects the default; negative disables
	// the gate entirely.
	ForegroundGrace time.Duration
}
type Engine struct {
	dispatchMu sync.RWMutex
	dispatcher *backgroundDispatcher
	schedule   backgroundSchedule
	cycleGate  chan struct{}
	store      *store.Store
	source     Source
	publisher  Publisher
	deps       Dependencies
	opt        Options
	locks      sync.Map
	retryMu    sync.Mutex
	retries    map[model.JobID]retryState
	foreground foregroundGate
}

func New(d Dependencies, o Options) (*Engine, error) {
	if d.Store == nil || d.Publisher == nil {
		return nil, errors.New("store and managed publisher required")
	}
	if d.Source == nil {
		if d.Cloud == nil {
			return nil, errors.New("managed cloud required")
		}
		d.Source = cloudSource{d.Cloud}
	}
	if _, err := d.Store.Scope(context.Background()); err != nil {
		return nil, err
	}
	if o.BackgroundTimeout <= 0 {
		o.BackgroundTimeout = 90 * time.Second
	}
	if o.TransferTimeout <= 0 {
		o.TransferTimeout = 20 * time.Minute
	}
	if o.InteractiveTimeout <= 0 {
		o.InteractiveTimeout = 45 * time.Second
	}
	if o.SliceOperations <= 0 {
		o.SliceOperations = 3
	}
	if o.SliceDuration <= 0 {
		o.SliceDuration = 30 * time.Second
	}
	if o.WriteBack < 0 {
		return nil, errors.New("write-back delay must not be negative")
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 3
	}
	if o.Concurrency > 16 {
		return nil, errors.New("concurrency out of range")
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 5 * time.Second
	}
	if o.DirectoryInterval <= 0 {
		o.DirectoryInterval = 30 * time.Second
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Engine{cycleGate: gate, store: d.Store, source: d.Source, publisher: d.Publisher, deps: d, opt: o, retries: map[model.JobID]retryState{}}, nil
}
func (e *Engine) entryLock(ctx context.Context, id model.EntryID) (func(), error) {
	candidate := make(chan struct{}, 1)
	candidate <- struct{}{}
	raw, _ := e.locks.LoadOrStore(id, candidate)
	ch := raw.(chan struct{})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ch:
		return func() { ch <- struct{}{} }, nil
	}
}
func (e *Engine) SyncEntry(ctx context.Context, id model.EntryID) error {
	return e.syncEntry(ctx, id, nil, false)
}
func (e *Engine) syncEntry(ctx context.Context, id model.EntryID, maxGeneration *uint64, skipWriteBack bool) error {
	if err := e.check(ctx); err != nil {
		return err
	}
	var unlock func()
	var err error
	if _, background := ctx.Value(quantumKey{}).(dispatchQuantum); background {
		candidate := make(chan struct{}, 1)
		candidate <- struct{}{}
		raw, _ := e.locks.LoadOrStore(id, candidate)
		ch := raw.(chan struct{})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			unlock = func() { ch <- struct{}{} }
		default:
			return errEntryBusy
		}
	} else {
		unlock, err = e.entryLock(ctx, id)
		if err != nil {
			return err
		}
	}
	defer unlock()
	if e.deps.ShouldSync != nil {
		ok, err := e.deps.ShouldSync(ctx, id)
		if err != nil || !ok {
			return err
		}
	}
	if err := e.store.SupersedeUnsentPredecessors(ctx, id); err != nil {
		return err
	}
	pending, err := e.store.Pending(ctx)
	if err != nil {
		return err
	}
	var job *model.Job
	for i := range pending {
		if pending[i].EntryID == id {
			job = &pending[i]
			break
		}
	}
	if job == nil {
		return e.publishPending(ctx, id)
	}
	if maxGeneration != nil && job.Generation > *maxGeneration {
		return nil
	}
	if !e.retryReady(job.ID) {
		return nil
	}
	if job.State == model.StatusPaused {
		journal, err := e.store.Journal(ctx, job.ID)
		if err != nil {
			return err
		}
		if !transient(journal.ErrorCode) {
			return nil
		}
		if err = e.store.ResumeJob(ctx, job.ID); err != nil {
			return err
		}
		refreshed, err := e.store.Job(ctx, job.ID)
		if err != nil {
			return err
		}
		job = &refreshed
	}
	if job.State == model.StatusConflict {
		next, err := e.retryCompatibilityConflict(ctx, *job)
		if err != nil {
			return err
		}
		job = &next
	}
	if job.State == model.StatusBlocked || job.State == model.StatusConflict {
		return nil
	}
	entry, err := e.store.Entry(ctx, id)
	if err != nil {
		return err
	}
	if entry.FrozenBy != "" {
		return store.ErrFrozen
	}
	if job.State == model.StatusQueued {
		ready, err := true, error(nil)
		if !skipWriteBack {
			ready, err = e.writeBackReady(ctx, *job)
		}
		if err != nil || !ready {
			return err
		}
		j, en, err := e.store.Prepare(ctx, job.ID)
		if err != nil {
			return err
		}
		job = &j
		entry = en
	}
	{
		entry, err = e.create(ctx, *job, entry)
		if err != nil {
			return err
		}
	}
	updated, err := e.store.Job(ctx, job.ID)
	if err != nil {
		return err
	}
	job = &updated
	if entry.Kind == model.KindDocx {
		return e.syncDoc(ctx, *job, entry)
	}
	if entry.Kind == model.KindFile {
		return e.syncFile(ctx, *job, entry)
	}
	return model.ErrUnsupportedContent
}

// Only new, unplanned queued work observes the write-back delay. Reconciliation
// of an earlier dispatch must not be delayed, and legacy journals have no clock.
func (e *Engine) writeBackReady(ctx context.Context, job model.Job) (bool, error) {
	if e.opt.WriteBack == 0 {
		return true, nil
	}
	journal, err := e.store.Journal(ctx, job.ID)
	if err != nil {
		return false, err
	}
	if journal.StagedAt.IsZero() || journal.Job.Plan != "" || len(journal.Job.OperationIDs) != 0 || len(journal.Operations) != 0 || len(journal.Results) != 0 || journal.Desired != "" {
		return true, nil
	}
	creates, err := e.store.PendingCreates(ctx)
	if err != nil {
		return false, err
	}
	for _, op := range creates {
		if op.JobID == job.ID {
			return true, nil
		}
	}
	return !e.now().Before(journal.StagedAt.Add(e.opt.WriteBack)), nil
}

func (e *Engine) publishPending(ctx context.Context, id model.EntryID) error {
	list, err := e.store.PendingPublications(ctx)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.EntryID == id {
			if err = e.publisher.Publish(ctx, c); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) confirm(ctx context.Context, j model.Job, sn model.Snapshot) error {
	c, err := e.store.Confirm(ctx, j.ID, sn)
	if err != nil {
		return err
	}
	return e.publisher.Publish(ctx, c)
}

func (e *Engine) check(ctx context.Context) error {
	if e.deps.CheckIdentity != nil {
		return e.deps.CheckIdentity(ctx)
	}
	return ctx.Err()
}
