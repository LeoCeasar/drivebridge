package manager

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
)

// Accepting a merge is rechecked against the cloud once, by the worker's resolve
// step. The version read that precedes it must be local: refreshing the conflict
// there too doubles the lark-cli processes every accepted file queues on the
// account lock, which is what made the second file of a batch stall.
func TestConflictForResolveDoesNotRefreshCloud(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	entry, err := w.services.store.EnsureEntry(ctx, "conflict.txt", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err = w.services.store.Stage(ctx, entry.ID, nil, strings.NewReader("local content"))
	if err != nil {
		t.Fatal(err)
	}
	conflictID := model.ConflictID(uuid.NewString())
	if err = w.services.store.SaveConflict(ctx, model.Conflict{ID: conflictID, EntryID: entry.ID, Generation: entry.Generation, Local: entry.Local, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int32
	w.mu.Lock()
	w.services.refreshConflict = func(context.Context, model.ConflictID) (model.Conflict, error) {
		refreshes.Add(1)
		return model.Conflict{}, nil
	}
	w.mu.Unlock()

	local, err := m.ConflictForResolve(ctx, conflictID)
	if err != nil || refreshes.Load() != 0 {
		t.Fatalf("local read refreshed the cloud: refreshes=%d err=%v", refreshes.Load(), err)
	}
	if local.Conflict.ID != conflictID || local.Entry.ID != entry.ID || local.Conflict.Generation != entry.Generation || local.Stale {
		t.Fatalf("local read lost the stored versions: %+v", local)
	}
	if _, err = m.Conflict(ctx, conflictID); err != nil || refreshes.Load() != 1 {
		t.Fatalf("opening a conflict must still refresh the cloud once: refreshes=%d err=%v", refreshes.Load(), err)
	}
}
