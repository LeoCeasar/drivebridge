package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

type preparationBackend struct {
	*fakeBackend
	reads     atomic.Int32
	refreshes atomic.Int32
}

func (b *preparationBackend) Conflict(ctx context.Context, id model.ConflictID) (ConflictInputs, error) {
	b.reads.Add(1)
	return b.fakeBackend.Conflict(ctx, id)
}
func (b *preparationBackend) RefreshConflict(ctx context.Context, id model.ConflictID) (ConflictInputs, error) {
	b.refreshes.Add(1)
	return b.fakeBackend.Conflict(ctx, id)
}

func TestPreparedConflictOpensCapturedInputsAndExplicitRefresh(t *testing.T) {
	h, b, c, _, _ := fixture(t)
	backend := &preparationBackend{fakeBackend: b}
	h.backend = backend
	summary := ConflictSummary{ID: b.input.Conflict.ID, Revision: conflictRevision(b.input)}
	h.prepareConflicts([]ConflictSummary{summary}, false)
	deadline := time.Now().Add(2 * time.Second)
	for !h.prepared.has(summary.ID, summary.Revision) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !h.prepared.has(summary.ID, summary.Revision) {
		t.Fatal("conflict was not prepared in the background")
	}
	for _, suffix := range []string{"", "?refresh=1"} {
		res := request(t, c, h, "GET", "/api/conflicts/"+string(summary.ID)+suffix, nil, "", "")
		var view ConflictView
		err := json.NewDecoder(res.Body).Decode(&view)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || view.Local != string(b.input.Local) || len(view.Hunks) == 0 {
			t.Fatalf("prepared view: %+v %v %s", view, err, res.Status)
		}
	}
	if backend.reads.Load() != 1 || backend.refreshes.Load() != 1 {
		t.Fatalf("ordinary open must use captured cache: reads=%d refreshes=%d", backend.reads.Load(), backend.refreshes.Load())
	}
}

func TestConflictRevisionIncludesDraftAndAllCapturedSources(t *testing.T) {
	_, b, _, _, _ := fixture(t)
	original := conflictRevision(b.input)
	for _, change := range []func(*ConflictInputs){
		func(in *ConflictInputs) { in.Conflict.Base = "new-base" }, func(in *ConflictInputs) { in.Conflict.Local = "new-local" }, func(in *ConflictInputs) { in.Conflict.Remote = "new-remote" }, func(in *ConflictInputs) { in.Conflict.Draft = "new-draft" }, func(in *ConflictInputs) { in.Conflict.Generation++ }, func(in *ConflictInputs) { in.Conflict.RemoteRevision = "new-revision" }, func(in *ConflictInputs) { in.Entry.Generation++ },
	} {
		next := b.input
		change(&next)
		if conflictRevision(next) == original {
			t.Fatal("changed conflict input kept old fingerprint")
		}
	}
}

type blockingPreparationBackend struct {
	*fakeBackend
	started chan model.ConflictID
	release chan struct{}
}

func (b *blockingPreparationBackend) Conflict(ctx context.Context, id model.ConflictID) (ConflictInputs, error) {
	b.started <- id
	select {
	case <-ctx.Done():
		return ConflictInputs{}, ctx.Err()
	case <-b.release:
	}
	in := b.input
	in.Conflict.ID = id
	return in, nil
}
func TestPreparationBoundsConcurrencyAndInvalidatesInflightSources(t *testing.T) {
	_, base, _, _, _ := fixture(t)
	b := &blockingPreparationBackend{fakeBackend: base, started: make(chan model.ConflictID, 100), release: make(chan struct{})}
	h := &Host{backend: b}
	var summaries []ConflictSummary
	for i := 0; i < 100; i++ {
		in := base.input
		in.Conflict.ID = model.ConflictID(fmt.Sprintf("conflict-%03d", i))
		summaries = append(summaries, ConflictSummary{ID: in.Conflict.ID, Revision: conflictRevision(in)})
	}
	h.prepareConflicts(summaries, false)
	first := <-b.started
	<-b.started
	select {
	case <-b.started:
		t.Fatal("more than two preparation workers")
	case <-time.After(20 * time.Millisecond):
	}
	h.prepared.mu.Lock()
	if len(h.prepared.pending) > preparedMaxEntries {
		t.Fatal("unbounded preparation queue")
	}
	h.prepared.mu.Unlock()
	// Removing a conflict while its read is in flight must prevent stale insertion.
	h.prepareConflicts(nil, false)
	close(b.release)
	deadline := time.Now().Add(time.Second)
	for {
		h.prepared.mu.Lock()
		workers := h.prepared.workers
		h.prepared.mu.Unlock()
		if workers == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("workers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := h.prepared.get(first); ok {
		t.Fatal("removed in-flight source re-entered cache")
	}
}
func TestPreparedCacheBoundsEntriesAndBytes(t *testing.T) {
	var cache preparedConflicts
	cache.init()
	for i := 0; i < preparedMaxEntries+5; i++ {
		id := model.ConflictID(fmt.Sprintf("id-%d", i))
		cache.desired[id] = "v"
		cache.put(id, "v", ConflictView{ID: id, Local: "text"})
	}
	if len(cache.entries) != preparedMaxEntries {
		t.Fatalf("cached %d entries", len(cache.entries))
	}
	big := strings.Repeat("x", 8<<20)
	for i := 0; i < 6; i++ {
		id := model.ConflictID(fmt.Sprintf("big-%d", i))
		cache.desired[id] = "v"
		cache.put(id, "v", ConflictView{ID: id, Local: big})
	}
	if cache.bytes > preparedMaxBytes || len(cache.entries) >= 6 {
		t.Fatalf("unbounded cache: %d bytes %d entries", cache.bytes, len(cache.entries))
	}
	// A changed fingerprint cannot use the old prepared result.
	cache.desired["big-5"] = "new"
	if _, ok := cache.get("big-5"); ok {
		t.Fatal("changed source reused old cache")
	}
}
