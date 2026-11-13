package vfscache

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNamespaceCheckpointDoesNotWaitForArchive(t *testing.T) {
	c, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved local bytes"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	entered, leave := make(chan struct{}), make(chan struct{})
	f.gate = func() { close(entered); <-leave }
	done := make(chan error, 1)
	go func() { done <- i.stageManagedJournal(context.Background(), nil) }()
	<-entered
	defer func() { close(leave); require.NoError(t, <-done) }()
	refsDone := make(chan error, 1)
	go func() {
		refs, err := c.CheckpointManagedTree(context.Background(), "note.md")
		if err == nil {
			require.Len(t, refs, 1)
			require.NotEmpty(t, refs[0].Journal)
			require.Equal(t, int64(len("saved local bytes")), refs[0].Size)
		}
		refsDone <- err
	}()
	select {
	case err := <-refsDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("local namespace checkpoint waited for background archive")
	}
}

func TestNamespaceDetachRetainsUnarchivedCheckpoint(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("recover this after unlink"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	require.NoError(t, i.Close(nil))
	refs, err := c.CheckpointManagedTree(context.Background(), "note.md")
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.NoError(t, c.DetachManagedTree("note.md"))
	require.NotContains(t, c.item, "note.md")
	detached := c.item[detachedManagedName(refs[0].Journal)]
	require.NotNil(t, detached)
	require.True(t, detached.info.Detached)
	require.NoError(t, detached.Open(nil))
	require.Equal(t, "recover this after unlink", readJournal(t, detached))
	require.NoError(t, detached.Close(nil))
	pending, err := ManagedMetadataPending(c.root, c.metaRoot)
	require.NoError(t, err)
	require.True(t, pending)
}

func TestCommittedNamespaceRecoversPendingJournalBinding(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "rename", true: "unlink"}[remove], func(t *testing.T) {
			c, i, _ := journalItem(t, "")
			_, err := i.WriteAt([]byte("durable without immutable import"), 0)
			require.NoError(t, err)
			flushCheckpoint(t, i)
			require.NoError(t, i.Close(nil))
			refs, err := c.CheckpointManagedTree(context.Background(), "note.md")
			require.NoError(t, err)
			i.closeManagedJournal()
			destination := "renamed.md"
			if remove {
				destination = ""
			}
			require.NoError(t, c.recoverManagedNamespace("note.md", destination, refs))
			c.mu.Lock()
			c.item = map[string]*Item{}
			c.mu.Unlock()
			name := destination
			if remove {
				name = detachedManagedName(refs[0].Journal)
			}
			recovered, _ := c.get(name)
			require.NoError(t, recovered.Open(nil))
			require.Equal(t, remove, recovered.info.Detached)
			require.Equal(t, "durable without immutable import", readJournal(t, recovered))
			require.NoError(t, recovered.Close(nil))
			recovered.closeManagedJournal()
		})
	}
}

func TestNamespacePinsPreventCheckpointRetirementUntilFinish(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("already archived but still needed by rename"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	require.NoError(t, i.stageManagedJournal(context.Background(), nil))
	require.NoError(t, i.Close(nil))
	refs, release, err := c.PinManagedNamespaceTree(context.Background(), "note.md")
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.NoError(t, i.compactManagedJournal(context.Background()))
	require.NotEmpty(t, i.info.Journal, "a captured namespace input must retain its WAL")
	removed, _ := i.RemoveNotInUse(0, false)
	require.False(t, removed)
	reset, _, err := i.Reset()
	require.NoError(t, err)
	require.Equal(t, SkippedPendingAccess, reset)
	release()
	require.NoError(t, i.compactManagedJournal(context.Background()))
	require.Empty(t, i.info.Journal)
}

func TestReplayRepairsAlreadyMovedDetachedMetadataAndSyncsData(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved before interrupted unlink publication"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	require.NoError(t, i.Close(nil))
	refs, err := c.CheckpointManagedTree(context.Background(), "note.md")
	require.NoError(t, err)
	require.Len(t, refs, 1)
	i.closeManagedJournal()
	target := detachedManagedName(refs[0].Journal)
	require.NoError(t, rename(c.toOSPath("note.md"), c.toOSPath(target)))
	require.NoError(t, rename(c.toOSPathMeta("note.md"), c.toOSPathMeta(target)))
	synced := map[string]bool{}
	c.managedIO.syncDir = func(p string) error { synced[p] = true; return nil }
	require.NoError(t, c.recoverManagedNamespace("note.md", "", refs))
	raw, err := os.ReadFile(c.toOSPathMeta(target))
	require.NoError(t, err)
	var info Info
	require.NoError(t, json.Unmarshal(raw, &info))
	require.True(t, info.Detached)
	require.True(t, synced[filepath.Dir(c.toOSPath("note.md"))], "source data directory must be durable before Finish")
	require.True(t, synced[filepath.Dir(c.toOSPath(target))], "target data directory must be durable before Finish")
}
