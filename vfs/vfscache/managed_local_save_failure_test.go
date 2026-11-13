package vfscache

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type localSaveRecovery interface {
	ManagedLocalSaveFailures() []string
	RetryManagedLocalSaves(context.Context) error
}

func localSaveRecoveryFor(t *testing.T, c *Cache) localSaveRecovery {
	t.Helper()
	r, ok := any(c).(localSaveRecovery)
	require.True(t, ok, "cache must expose local checkpoint failure and local-only retry")
	return r
}

func TestLocalCheckpointRetryWakesOnlyAfterConfirmation(t *testing.T) {
	c, i, _ := journalItem(t, "")
	c.managedWake = make(chan struct{}, 1)
	assertIdle := func() {
		t.Helper()
		select {
		case <-c.managedWake:
			t.Fatal("no-op or failed local retry must not wake the archive worker")
		default:
		}
	}
	require.NoError(t, c.RetryManagedLocalSaves(context.Background()))
	assertIdle()
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(file *os.File) error {
		if file == i.journal.file {
			return syscall.EIO
		}
		return real(file)
	}
	require.ErrorIs(t, i.Close(nil), syscall.EIO)
	// Close wakes the worker independently of the retry under test.
	<-c.managedWake
	require.ErrorIs(t, c.RetryManagedLocalSaves(context.Background()), syscall.EIO)
	assertIdle()
	c.managedIO.syncFile = real
	require.NoError(t, c.RetryManagedLocalSaves(context.Background()))
	select {
	case <-c.managedWake:
	default:
		t.Fatal("accepted local checkpoint must wake the archive worker")
	}
	require.NoError(t, c.RetryManagedLocalSaves(context.Background()))
	assertIdle()
}

func TestLocalCheckpointClosedFailureIsVisibleAndRetriesWithoutRewrite(t *testing.T) {
	c, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("first"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	_, err = i.WriteAt([]byte("later"), 0)
	require.NoError(t, err)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(file *os.File) error {
		if file == i.journal.file {
			return syscall.ENOSPC
		}
		return real(file)
	}
	require.ErrorIs(t, i.Close(nil), syscall.ENOSPC)
	require.Nil(t, i.fd, "close must release the body even when checkpoint fails")
	saved, _, archiveFailures := c.ManagedLocalSaveStatus()
	require.False(t, saved)
	require.Empty(t, archiveFailures, "WAL confirmation failure is not an archive failure")
	r := localSaveRecoveryFor(t, c)
	require.Equal(t, []string{"note.md"}, r.ManagedLocalSaveFailures())
	before, err := i.journal.file.Stat()
	require.NoError(t, err)
	require.ErrorIs(t, r.RetryManagedLocalSaves(context.Background()), syscall.ENOSPC)
	after, err := i.journal.file.Stat()
	require.NoError(t, err)
	require.Equal(t, before.Size(), after.Size(), "repeated fsync failure must not grow the WAL")
	require.Equal(t, []string{"note.md"}, r.ManagedLocalSaveFailures(), "failed retry must retain the issue")
	require.True(t, c.ManagedPending())
	require.True(t, i.inUse(), "unsaved bytes must remain protected")
	require.Empty(t, f.updates, "local retry must not import or contact a backend")
	c.managedIO.syncFile = real
	require.NoError(t, r.RetryManagedLocalSaves(context.Background()))
	require.Empty(t, r.ManagedLocalSaveFailures())
	saved, preparing, archiveFailures := c.ManagedLocalSaveStatus()
	require.True(t, saved)
	require.Equal(t, 1, preparing)
	require.Empty(t, archiveFailures)
	require.Empty(t, f.updates)
	// Both acknowledged saves remain available to the immutable archive queue.
	require.NoError(t, c.DrainManagedArchives(context.Background()))
	require.Equal(t, []string{"first", "later"}, f.updates)
}

func TestLocalCheckpointRetryDoesNotAcceptActiveOrNeverSavedWrites(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("active"), 0)
	require.NoError(t, err)
	r := localSaveRecoveryFor(t, c)
	require.NoError(t, r.RetryManagedLocalSaves(context.Background()))
	require.Zero(t, i.info.DurableSeq)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(*os.File) error { return syscall.EIO }
	require.ErrorIs(t, i.Sync(), syscall.EIO, "explicit Sync must propagate the failure")
	require.Equal(t, []string{"note.md"}, r.ManagedLocalSaveFailures())
	c.managedIO.syncFile = real
	require.NoError(t, r.RetryManagedLocalSaves(context.Background()))
	require.Zero(t, i.info.DurableSeq, "background retry cannot checkpoint an active writer")
	require.Equal(t, []string{"note.md"}, r.ManagedLocalSaveFailures())
	flushCheckpoint(t, i)
	require.Empty(t, r.ManagedLocalSaveFailures(), "explicit successful save clears the issue")
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointArchiveSyncFailureDoesNotBecomeSaveFailure(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(*os.File) error { return syscall.ENOSPC }
	require.ErrorIs(t, c.DrainManagedArchives(context.Background()), syscall.ENOSPC)
	saved, _, archiveFailures := c.ManagedLocalSaveStatus()
	require.True(t, saved)
	require.Equal(t, []string{"note.md"}, archiveFailures)
	require.Empty(t, localSaveRecoveryFor(t, c).ManagedLocalSaveFailures())
	c.managedIO.syncFile = real
	require.NoError(t, c.DrainManagedArchives(context.Background()))
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointWorkerRetriesClosedFailure(t *testing.T) {
	c, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	wal := i.journal.file
	real := c.managedIO.syncFile
	var failing atomic.Bool
	failing.Store(true)
	c.managedIO.syncFile = func(file *os.File) error {
		if file == wal && failing.Load() {
			return syscall.EIO
		}
		return real(file)
	}
	require.ErrorIs(t, i.Close(nil), syscall.EIO)
	ctx, cancel := context.WithCancel(context.Background())
	c.managedCancel = cancel
	c.managedWake = make(chan struct{}, 1)
	c.managedDone = make(chan struct{})
	go c.managedJournalWorker(ctx)
	t.Cleanup(c.StopManagedArchives)
	c.wakeManagedWorker()
	require.Eventually(t, func() bool { return len(c.ManagedLocalSaveFailures()) == 1 }, time.Second, time.Millisecond)
	failing.Store(false)
	c.wakeManagedWorker()
	require.Eventually(t, func() bool {
		saved, _, _ := c.ManagedLocalSaveStatus()
		return saved && len(c.ManagedLocalSaveFailures()) == 0
	}, 3*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.updates) == 1 && f.updates[0] == "saved"
	}, 3*time.Second, time.Millisecond)
}

func TestLocalCheckpointClosedRetryDoesNotWaitForArchive(t *testing.T) {
	c, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("first"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.gate = func() { once.Do(func() { close(entered); <-leave }) }
	done := make(chan error, 1)
	go func() { done <- c.DrainManagedArchives(context.Background()) }()
	<-entered
	_, err = i.WriteAt([]byte("later"), 0)
	require.NoError(t, err)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(file *os.File) error {
		if file == i.journal.file {
			return syscall.EIO
		}
		return real(file)
	}
	require.ErrorIs(t, i.Close(nil), syscall.EIO)
	c.managedIO.syncFile = real
	retried := make(chan error, 1)
	go func() { retried <- c.RetryManagedLocalSaves(context.Background()) }()
	select {
	case err := <-retried:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		close(leave)
		<-done
		t.Fatal("closed WAL retry waited for background archive")
	}
	close(leave)
	require.NoError(t, <-done)
	require.Equal(t, []string{"first", "later"}, f.updates)
}
