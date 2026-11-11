package manager

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/vfs"
)

type sweepAttempt struct {
	id         model.EntryID
	generation uint64
	access     []time.Time
}
type sweepStore struct {
	candidates    []store.CacheCandidate
	retained      int64
	freed         map[model.EntryID]int64
	currentAccess map[model.EntryID]time.Time
	attempts      []sweepAttempt
	gcCalls       int
	afterCommit   func()
	commitErr     error
}

func (s *sweepStore) GC(ctx context.Context, _ store.GCOptions) (store.GCResult, error) {
	s.gcCalls++
	if err := ctx.Err(); err != nil {
		return store.GCResult{}, err
	}
	return store.GCResult{RetainedBytes: s.retained}, nil
}
func (s *sweepStore) CleanCacheCandidates(ctx context.Context) ([]store.CacheCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]store.CacheCandidate(nil), s.candidates...), nil
}
func (s *sweepStore) EvictCleanEntry(ctx context.Context, id model.EntryID, generation uint64, access ...time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.attempts = append(s.attempts, sweepAttempt{id, generation, append([]time.Time(nil), access...)})
	if at, ok := s.currentAccess[id]; ok && (len(access) != 1 || !access[0].Equal(at)) {
		return false, nil
	}
	s.retained -= s.freed[id]
	if s.afterCommit != nil {
		s.afterCommit()
	}
	return true, s.commitErr
}

type sweepVFS struct {
	bytes            int64
	busy             map[string]bool
	invalidate       map[string]int64
	paths, published []string
}

func (v *sweepVFS) ManagedCacheBytes() int64 { return v.bytes }
func (v *sweepVFS) TryExternalUpdate(ctx context.Context, name string, commit func() (fs.Object, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.paths = append(v.paths, name)
	if v.busy[name] {
		return vfs.ErrExternalUpdateBusy
	}
	v.bytes -= v.invalidate[name]
	object, err := commit()
	if err != nil {
		return err
	}
	if object == nil {
		return errors.New("missing placeholder")
	}
	v.published = append(v.published, name)
	return nil
}

type sweepObject struct {
	fs.Object
	name string
}

func (o sweepObject) Remote() string { return o.name }
func sweepPublisher(ctx context.Context, name string) (fs.Object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sweepObject{name: name}, nil
}
func cacheCandidate(id, name string, at time.Time) store.CacheCandidate {
	return store.CacheCandidate{EntryID: model.EntryID(id), Remote: name, Generation: 7, AccessedAt: at}
}

func TestBodySweepAgeKeepsRecentAndPublishesPlaceholder(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	old := cacheCandidate("old", "old.md", now.Add(-8*24*time.Hour))
	recent := cacheCandidate("recent", "recent.md", now.Add(-24*time.Hour))
	s := &sweepStore{candidates: []store.CacheCandidate{recent, old}, retained: 200, freed: map[model.EntryID]int64{"old": 100}}
	v := &sweepVFS{}
	result, err := sweepCleanBodies(context.Background(), s, v, sweepPublisher, 7*24*time.Hour, 1000, now)
	if err != nil || result.EvictedEntries != 1 || result.BytesBefore != 200 || result.BytesAfter != 100 || result.OverBudget {
		t.Fatal(result, err)
	}
	if !reflect.DeepEqual(v.paths, []string{"old.md"}) || !reflect.DeepEqual(v.published, v.paths) {
		t.Fatal(v)
	}
	if len(s.attempts) != 1 || s.attempts[0].id != "old" || s.attempts[0].generation != old.Generation || len(s.attempts[0].access) != 1 || !s.attempts[0].access[0].Equal(old.AccessedAt) {
		t.Fatal("missing generation/access CAS", s.attempts)
	}
	if s.gcCalls != 2 {
		t.Fatal("must collect before accounting and after eviction", s.gcCalls)
	}
}

func TestBodySweepCombinedQuotaSkipsBusyAndHandlesSharedZeroBytes(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	s := &sweepStore{retained: 80, freed: map[model.EntryID]int64{"shared-b": 80}, candidates: []store.CacheCandidate{
		cacheCandidate("recent", "recent.md", now), cacheCandidate("shared-b", "b.md", now.Add(-time.Hour)), cacheCandidate("busy", "busy.md", now.Add(-3*time.Hour)), cacheCandidate("shared-a", "a.md", now.Add(-time.Hour)),
	}}
	v := &sweepVFS{bytes: 80, busy: map[string]bool{"busy.md": true}, invalidate: map[string]int64{"a.md": 20, "b.md": 20}}
	result, err := sweepCleanBodies(context.Background(), s, v, sweepPublisher, 0, 90, now)
	if err != nil || result.EvictedEntries != 2 || result.BytesBefore != 160 || result.BytesAfter != 40 || result.OverBudget {
		t.Fatal(result, err)
	}
	if !reflect.DeepEqual(v.paths, []string{"busy.md", "a.md", "b.md"}) {
		t.Fatal("LRU, remote tie ordering, or busy skip failed", v.paths)
	}
	if len(s.attempts) != 2 || s.attempts[0].id != "shared-a" || s.attempts[1].id != "shared-b" {
		t.Fatal("zero reclaim estimate must not suppress shared candidates", s.attempts)
	}
}

func TestBodySweepAccessCASProtectsNewUseAndQuotaIsSoft(t *testing.T) {
	now := time.Now().UTC()
	candidate := cacheCandidate("entry", "file.md", now.Add(-10*24*time.Hour))
	s := &sweepStore{retained: 100, candidates: []store.CacheCandidate{candidate}, freed: map[model.EntryID]int64{"entry": 100}, currentAccess: map[model.EntryID]time.Time{"entry": now}}
	v := &sweepVFS{}
	result, err := sweepCleanBodies(context.Background(), s, v, sweepPublisher, 24*time.Hour, 50, now)
	if err != nil || result.EvictedEntries != 0 || result.BytesAfter != 100 || !result.OverBudget {
		t.Fatal(result, err)
	}
	if len(s.attempts) != 1 || len(s.attempts[0].access) != 1 || !s.attempts[0].access[0].Equal(candidate.AccessedAt) {
		t.Fatal("candidate timestamp not passed to CAS", s.attempts)
	}
	protected := &sweepStore{retained: 100, candidates: []store.CacheCandidate{candidate}}
	blocked := &sweepVFS{busy: map[string]bool{"file.md": true}}
	result, err = sweepCleanBodies(context.Background(), protected, blocked, sweepPublisher, 0, 50, now)
	if err != nil || !result.OverBudget || result.EvictedEntries != 0 || len(protected.attempts) != 0 {
		t.Fatal("busy content was altered to force quota", result, err, protected.attempts)
	}
	// Releasing a clean entry does not override the Store's 24-hour prepared
	// grace or other retained references. Actual post-GC bytes decide the result.
	grace := &sweepStore{retained: 100, candidates: []store.CacheCandidate{candidate}}
	result, err = sweepCleanBodies(context.Background(), grace, &sweepVFS{}, sweepPublisher, 0, 50, now)
	if err != nil || result.EvictedEntries != 1 || result.BytesAfter != 100 || !result.OverBudget {
		t.Fatal("quota incorrectly assumed unlinks or forced protected-byte removal", result, err)
	}
	onlyCapacity := &sweepStore{retained: 100, candidates: []store.CacheCandidate{candidate}}
	result, err = sweepCleanBodies(context.Background(), onlyCapacity, &sweepVFS{}, sweepPublisher, 0, 200, now)
	if err != nil || result.EvictedEntries != 0 || len(onlyCapacity.attempts) != 0 {
		t.Fatal("maxAge zero must disable age eviction", result, err)
	}
}

func TestBodySweepCancellationStillPublishesCommittedPlaceholder(t *testing.T) {
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &sweepStore{retained: 100, candidates: []store.CacheCandidate{cacheCandidate("first", "a.md", now.Add(-time.Hour)), cacheCandidate("second", "b.md", now)}, freed: map[model.EntryID]int64{"first": 50}, afterCommit: cancel, commitErr: context.Canceled}
	v := &sweepVFS{}
	result, err := sweepCleanBodies(ctx, s, v, sweepPublisher, 0, 10, now)
	if !errors.Is(err, context.Canceled) || result.EvictedEntries != 1 || len(s.attempts) != 1 || !reflect.DeepEqual(v.published, []string{"a.md"}) {
		t.Fatal("cancel lost committed placeholder or touched another candidate", result, err, s.attempts, v.published)
	}
	already, cancelEarly := context.WithCancel(context.Background())
	cancelEarly()
	fresh := &sweepStore{retained: 100}
	if _, err := sweepCleanBodies(already, fresh, &sweepVFS{}, sweepPublisher, 0, 10, now); !errors.Is(err, context.Canceled) || fresh.gcCalls != 0 {
		t.Fatal("pre-cancelled sweep performed work", err, fresh.gcCalls)
	}
}
