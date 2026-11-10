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
	Store              *store.Store
	Cloud              *cloud.Client
	Source             Source
	Publisher          Publisher
	LocalOnly          func(string) bool
	ShouldSync         func(context.Context, model.EntryID) (bool, error)
	ActiveEntries      func() []model.EntryID
	RefreshDirectories func(context.Context) error
}
type Options struct {
	Now                             func() time.Time
	Concurrency                     int
	PollInterval, DirectoryInterval time.Duration
}
type Engine struct {
	store     *store.Store
	source    Source
	publisher Publisher
	deps      Dependencies
	opt       Options
	locks     sync.Map
	retryMu   sync.Mutex
	retries   map[model.JobID]retryState
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
	return &Engine{store: d.Store, source: d.Source, publisher: d.Publisher, deps: d, opt: o, retries: map[model.JobID]retryState{}}, nil
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
	unlock, err := e.entryLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if e.deps.ShouldSync != nil {
		ok, err := e.deps.ShouldSync(ctx, id)
		if err != nil || !ok {
			return err
		}
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
