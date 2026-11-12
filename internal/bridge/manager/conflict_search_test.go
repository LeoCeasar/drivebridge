package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestSaveDraftFindsLiveTargetAfterLockedOfflineMount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, offlineID, _, _ := fixture(t, nil)
	_, offline := offlineInstallationStore(t, m, offlineID)
	if err := offline.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	live := c.Mounts[0]
	live.ID = model.MountID(uuid.NewString())
	live.Name = "live-target"
	live.Mountpoint = filepath.Join(filepath.Dir(live.Mountpoint), "live-target")
	if err = validateMountpoint(live.Mountpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = m.AddMount(ctx, live); err != nil {
		t.Fatal(err)
	}
	m.spawn = func(_ context.Context, args []string, _ string) (int, error) {
		id := model.MountID(args[len(args)-1])
		go func() { _ = m.RunWorker(ctx, id) }()
		return os.Getpid(), nil
	}
	if _, err = m.Start(ctx, live.ID, true); err != nil {
		t.Fatal(err)
	}
	target := m.localWorker(live.ID).services.store
	entry, err := target.EnsureEntry(ctx, "conflict.txt", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err = target.Stage(ctx, entry.ID, nil, strings.NewReader("local content"))
	if err != nil {
		t.Fatal(err)
	}
	conflictID := model.ConflictID(uuid.NewString())
	if err = target.SaveConflict(ctx, model.Conflict{ID: conflictID, EntryID: entry.ID, Generation: entry.Generation, Local: entry.Local, Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	lock, err := privateLock(m.runtimeLockPath(offlineID))
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := lock.TryLock()
	if err != nil || !acquired {
		t.Fatal(acquired, err)
	}
	defer lock.Unlock()
	if _, err = m.Conflicts(ctx, offlineID); !errors.Is(err, ErrPending) {
		t.Fatalf("offline contention not reproduced: %v", err)
	}
	if err = m.SaveDraft(ctx, conflictID, []byte("draft for live target")); err != nil {
		t.Fatalf("unrelated offline lock blocked live draft: %v", err)
	}
	conflicts, err := target.Conflicts(ctx)
	if err != nil || len(conflicts) != 1 {
		t.Fatal(conflicts, err)
	}
	reader, err := target.ReadBlob(ctx, conflicts[0].Draft)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(draft) != "draft for live target" {
		t.Fatalf("draft not saved to target: %q %v", draft, err)
	}
	unknown := model.ConflictID(uuid.NewString())
	if _, err = m.findConflict(ctx, unknown); !errors.Is(err, ErrPending) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("incomplete search silently reported not-found: %v", err)
	}
	if err = lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err = m.findConflict(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("complete search did not report not-found: %v", err)
	}
	// End the live fixture through its owned context; no native mount is involved.
	cancel()
	deadline := time.After(time.Second)
	for m.localWorker(live.ID) != nil {
		select {
		case <-deadline:
			t.Fatal("fixture worker did not exit")
		case <-time.After(time.Millisecond):
		}
	}
}
