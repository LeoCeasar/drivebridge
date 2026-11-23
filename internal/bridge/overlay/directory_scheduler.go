package overlay

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/store"
)

var ErrDirectoryWaiting = errors.New("directory listing is waiting for its retry time")

const directoryFreshFor = 30 * time.Second
const directoryReadBudget = 30 * time.Second
const maxDirectoryRequests = 256

type directoryRequest struct {
	entry           model.Entry
	readyAt         time.Time
	running, follow bool
	manual          bool
}
type directorySchedule struct {
	mu         sync.Mutex
	wake       chan struct{}
	requests   map[model.EntryID]*directoryRequest
	queue      []model.EntryID
	burst      int
	closed     bool
	active     map[uint64]context.CancelCauseFunc
	next       uint64
	done       chan struct{}
	doneClosed bool
}

func (s *directorySchedule) init() {
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
		s.requests = make(map[model.EntryID]*directoryRequest)
		s.active = make(map[uint64]context.CancelCauseFunc)
		s.done = make(chan struct{})
	}
}
func (s *directorySchedule) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// DirectoryRefreshWake belongs to the existing metadata worker. List never
// creates a network goroutine and never waits for the consumer of this channel.
func (f *Fs) DirectoryRefreshWake() <-chan struct{} {
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.wake
}

// RequestDirectoryRefresh accepts one scoped, explicit metadata observation.
// The entry identity is captured now and checked again by the reader.
func (f *Fs) RequestDirectoryRefresh(ctx context.Context, remote string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !model.ValidDirectoryRemote(remote) {
		return errors.New("invalid mount-relative directory")
	}
	if err := validPath(remote, true); err != nil {
		return err
	}
	if allowed, err := f.SyncPathAllowed(ctx, remote); err != nil {
		return err
	} else if !allowed {
		return store.ErrNotFound
	}
	en, err := f.directory(ctx, remote)
	if err != nil {
		return err
	}
	m, err := f.store.Metadata(ctx, en.ID)
	if err != nil {
		return err
	}
	return f.queueDirectoryRefresh(en, m, true)
}
func (f *Fs) queueDirectoryRefresh(en model.Entry, m store.Metadata, manual bool) error {
	now := time.Now()
	if !manual && !m.DirectoryVerifiedAt.IsZero() && now.Sub(m.DirectoryVerifiedAt) < directoryFreshFor {
		return nil
	}
	if !manual {
		f.refreshMu.Lock()
		pending := f.refreshing[en.ID]
		merged := pending != nil && pending.remote == en.Remote
		f.refreshMu.Unlock()
		if merged {
			return nil
		}
	}
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.closed {
		return context.Canceled
	}
	if old := s.requests[en.ID]; old != nil {
		if old.entry.Token != en.Token || old.entry.Remote != en.Remote {
			return model.ErrIdentityChanged
		}
		if manual {
			old.manual = true
		}
		if old.running && manual {
			old.follow = true
		}
		if !old.running && !now.Before(old.readyAt) {
			s.signal()
		}
		return nil
	}
	if len(s.requests) >= maxDirectoryRequests {
		return store.ErrBusy
	}
	request := &directoryRequest{entry: en, manual: manual}
	if m.DirectoryOnline != nil && !m.DirectoryOnline.Resolved {
		request.readyAt = m.DirectoryOnline.NotBefore
	}
	s.requests[en.ID] = request
	s.queue = append(s.queue, en.ID)
	s.signal()
	return nil
}
func (f *Fs) hasDirectoryRequests() bool {
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests) > 0 && !s.closed
}
func (f *Fs) takeDirectoryRequest(hasNormal bool) *directoryRequest {
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if s.closed || hasNormal && s.burst >= 3 {
		return nil
	}
	for remaining := len(s.queue); remaining > 0; remaining-- {
		id := s.queue[0]
		s.queue = s.queue[1:]
		r := s.requests[id]
		if r == nil || r.running {
			continue
		}
		if time.Now().Before(r.readyAt) {
			s.queue = append(s.queue, id)
			continue
		}
		r.running = true
		s.burst++
		return r
	}
	return nil
}
func (f *Fs) normalDirectoryTurn() {
	s := &f.directorySchedule
	s.mu.Lock()
	s.burst = 0
	s.mu.Unlock()
}
func (f *Fs) finishDirectoryRequest(ctx context.Context, r *directoryRequest) {
	m, err := f.store.Metadata(context.WithoutCancel(ctx), r.entry.ID)
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.requests[r.entry.ID] != r {
		return
	}
	waiting := err == nil && !m.Tombstone && m.DirectoryOnline != nil && !m.DirectoryOnline.Resolved && time.Now().Before(m.DirectoryOnline.NotBefore)
	if (r.follow || waiting) && !s.closed {
		r.running = false
		r.follow = false
		r.readyAt = time.Time{}
		if err != nil {
			r.readyAt = time.Now().Add(directoryFreshFor)
		} else if m.DirectoryOnline != nil && !m.DirectoryOnline.Resolved {
			r.readyAt = m.DirectoryOnline.NotBefore
		}
		s.queue = append(s.queue, r.entry.ID)
		s.signal()
	} else {
		delete(s.requests, r.entry.ID)
	}
}
func (f *Fs) directoryReadDue(ctx context.Context, id model.EntryID) (bool, error) {
	m, err := f.store.Metadata(ctx, id)
	if err != nil {
		return false, err
	}
	return !m.Tombstone && (m.DirectoryOnline == nil || m.DirectoryOnline.Resolved || !time.Now().Before(m.DirectoryOnline.NotBefore)), nil
}
func (f *Fs) beginDirectoryRead(ctx context.Context) (context.Context, func(), error) {
	base, cancel := context.WithCancelCause(ctx)
	read, timeout := context.WithTimeout(base, directoryReadBudget)
	s := &f.directorySchedule
	s.mu.Lock()
	s.init()
	if s.closed {
		s.mu.Unlock()
		cancel(online.CancellationCause("runtime_shutdown"))
		timeout()
		return nil, nil, context.Canceled
	}
	s.next++
	id := s.next
	s.active[id] = cancel
	s.mu.Unlock()
	release := func() {
		timeout()
		cancel(nil)
		s.mu.Lock()
		delete(s.active, id)
		if s.closed && len(s.active) == 0 && !s.doneClosed {
			close(s.done)
			s.doneClosed = true
		}
		s.mu.Unlock()
	}
	return read, release, nil
}
func (f *Fs) stopDirectoryReads(ctx context.Context) error {
	s := &f.directorySchedule
	s.mu.Lock()
	s.init()
	s.closed = true
	s.queue = nil
	clear(s.requests)
	for _, cancel := range s.active {
		cancel(online.CancellationCause("runtime_shutdown"))
	}
	if len(s.active) == 0 && !s.doneClosed {
		close(s.done)
		s.doneClosed = true
	}
	done := s.done
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RequestVisitedDirectoryRefresh is a local admission pass. It never performs
// network I/O and never lets a catalog update jump ahead of every opened parent.
func (f *Fs) RequestVisitedDirectoryRefresh(ctx context.Context) error {
	if err := f.RefreshCatalog(ctx); err != nil {
		return err
	}
	f.activityMu.Lock()
	ids := make([]model.EntryID, 0, len(f.listed))
	for id, visited := range f.listed {
		if visited {
			ids = append(ids, id)
		}
	}
	f.activityMu.Unlock()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		en, err := f.store.Entry(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		m, err := f.store.Metadata(ctx, id)
		if err != nil {
			return err
		}
		if m.Tombstone {
			continue
		}
		if err = f.queueDirectoryRefresh(en, m, true); errors.Is(err, store.ErrBusy) {
			break
		} else if err != nil {
			return err
		}
	}
	return nil
}

// DirectoryRefreshDelay is owned by the worker timer, not a detached scheduler
// goroutine. A service bound remains authoritative even with a long scan period.
func (f *Fs) DirectoryRefreshDelay() (time.Duration, bool) {
	if !f.hasDirectoryRequests() {
		return 0, false
	}
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest time.Time
	found := false
	for _, r := range s.requests {
		if r.running {
			continue
		}
		if !found || r.readyAt.Before(earliest) {
			earliest = r.readyAt
			found = true
		}
	}
	if !found {
		return 0, false
	}
	delay := time.Until(earliest)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func (f *Fs) forcedDirectoryRequest(r *directoryRequest) bool {
	s := &f.directorySchedule
	s.mu.Lock()
	defer s.mu.Unlock()
	return r.manual
}
