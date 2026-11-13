package integration

import (
	"context"
	"fmt"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/overlay"
	"github.com/rclone/rclone/vfs"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

type pausedCheckpointFS struct {
	*overlay.Fs
	entered, release chan struct{}
	once             sync.Once
}

func (f *pausedCheckpointFS) ArchiveManagedCheckpoint(ctx context.Context, key string, seq uint64, r io.Reader, src fs.ObjectInfo) (fs.Object, error) {
	f.once.Do(func() { close(f.entered) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.release:
	}
	return f.Fs.ArchiveManagedCheckpoint(ctx, key, seq, r, src)
}
func pauseArchives(t *testing.T, h *harness) (*pausedCheckpointFS, func()) {
	t.Helper()
	opts := h.vfs.Opt
	h.vfs.Shutdown()
	paused := &pausedCheckpointFS{Fs: h.overlay, entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(paused.release) }) }
	t.Cleanup(release)
	var err error
	h.vfs, err = vfs.NewManaged(h.ctx, paused, &opts)
	require.NoError(t, err)
	return paused, release
}
func saveWithoutArchive(t *testing.T, h *harness, name, body string) {
	t.Helper()
	f, err := h.vfs.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = f.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Flush())
	require.NoError(t, f.Close())
}
func TestRepeatedSafeSaveWhileArchivePausedPreservesLatestAndIdentity(t *testing.T) {
	h := newHarness(t)
	h.read("document.md")
	original := h.entry("document.md")
	paused, release := pauseArchives(t, h)
	saveWithoutArchive(t, h, "first.tmp", "first pending version")
	select {
	case <-paused.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive not paused")
	}
	durations := make([]time.Duration, 0, 100)
	latest := ""
	var saveTimes, renameTimes, readTimes []time.Duration
	for n := 0; n < 100; n++ {
		start := time.Now()
		temp := fmt.Sprintf("save-%03d.tmp", n)
		latest = fmt.Sprintf("# newest local version %d\n\nstable anchor 中文😀\n", n)
		saveWithoutArchive(t, h, temp, latest)
		savedAt := time.Now()
		saveTimes = append(saveTimes, savedAt.Sub(start))
		require.NoError(t, h.vfs.Rename(temp, "document.md"))
		renamedAt := time.Now()
		renameTimes = append(renameTimes, renamedAt.Sub(savedAt))
		require.Equal(t, latest, h.read("document.md"))
		_, err := h.vfs.ReadDir("")
		require.NoError(t, err)
		durations = append(durations, time.Since(start))
		readTimes = append(readTimes, time.Since(renamedAt))
	}
	current, err := h.store.EntryByRemote(h.ctx, "document.md")
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Equal(t, original.Token, current.Token)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("100 safe saves+open+readdir with archive paused: p50=%s p95=%s p99=%s max=%s", durations[49], durations[94], durations[98], durations[99])
	for name, times := range map[string][]time.Duration{"local_save": saveTimes, "atomic_replace": renameTimes, "read_and_list": readTimes} {
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		t.Logf("%s p50=%s p95=%s p99=%s", name, times[49], times[94], times[98])
	}
	release()
	require.NoError(t, h.vfs.DrainManagedArchives(h.ctx))
	current = h.entry("document.md")
	require.Equal(t, latest, h.blob(current.Local))
	syncErr := h.engine.SyncEntry(h.ctx, current.ID)
	if syncErr != nil {
		conflicts, _ := h.store.Conflicts(h.ctx)
		jobs, _ := h.store.Pending(h.ctx)
		t.Logf("sync failed: entry=%+v conflicts=%+v jobs=%+v", current, conflicts, jobs)
		for _, job := range jobs {
			if job.EntryID == current.ID {
				t.Logf("job local=%q base=%q", h.blob(job.Local), h.blob(job.BaseLocal))
			}
		}
	}
	require.NoError(t, syncErr)
	require.Equal(t, latest, h.cloud.doc(t, h.docToken))
}
func TestUnlinkDoesNotWaitForArchiveAndKeepsDeletedSaveHistory(t *testing.T) {
	h := newHarness(t)
	paused, release := pauseArchives(t, h)
	saveWithoutArchive(t, h, "remove.txt", "acknowledged saved bytes")
	select {
	case <-paused.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive not paused")
	}
	original, err := h.store.EntryByRemote(h.ctx, "remove.txt")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- h.vfs.Remove("remove.txt") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("unlink waited for archive")
	}
	_, err = h.vfs.Stat("remove.txt")
	require.Error(t, err)
	release()
	require.NoError(t, h.vfs.DrainManagedArchives(h.ctx))
	events, err := h.store.HistoryEvents(h.ctx, 0, 1000)
	require.NoError(t, err)
	found := false
	for _, event := range events {
		if event.Entry.ID == original.ID && event.Bodies["local"] != "" && h.blob(event.Bodies["local"]) == "acknowledged saved bytes" {
			found = true
		}
	}
	require.True(t, found, "unlinked saved bytes must reach history")
}
func TestRenamedPendingJournalSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	paused, _ := pauseArchives(t, h)
	saveWithoutArchive(t, h, "before.tmp", "restart survives complete local version")
	select {
	case <-paused.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("archive not paused")
	}
	require.NoError(t, h.vfs.Rename("before.tmp", "after.md"))
	h.restart()
	require.Equal(t, "restart survives complete local version", h.read("after.md"))
	require.NoError(t, h.vfs.DrainManagedArchives(h.ctx))
	require.Equal(t, "restart survives complete local version", h.blob(h.entry("after.md").Local))
}

func TestAtomicReplaceKeepsExistingReadHandlesUsable(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("reader_started_%t", started), func(t *testing.T) {
			h := newHarness(t)
			original := h.read("document.md")
			identity := h.entry("document.md")
			reader, err := h.vfs.Open("document.md")
			require.NoError(t, err)
			defer reader.Close()
			var prefix string
			if started {
				b := make([]byte, 5)
				_, err = io.ReadFull(reader, b)
				require.NoError(t, err)
				prefix = string(b)
			}
			replacement := "replacement while grep retains an old reader\n\nstable anchor 中文😀\n\ntail\n"
			saveWithoutArchive(t, h, "replace.tmp", replacement)
			require.NoError(t, h.vfs.Rename("replace.tmp", "document.md"), "a read-only grep handle must not reject an editor's atomic save")
			publishedNode, _ := h.vfs.StatMetadata("document.md")
			rest, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Equal(t, original, prefix+string(rest))
			currentNode, _ := h.vfs.StatMetadata("document.md")
			require.Same(t, publishedNode, currentNode, "old reader must not republish displaced inode")
			require.Equal(t, replacement, h.read("document.md"))
			require.NoError(t, h.vfs.DrainManagedArchives(h.ctx))
			current, err := h.store.EntryByRemote(h.ctx, "document.md")
			require.NoError(t, err)
			require.Equal(t, identity.ID, current.ID)
			require.Equal(t, identity.Token, current.Token)
		})
	}
}
