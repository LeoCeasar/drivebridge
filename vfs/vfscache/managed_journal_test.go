package vfscache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/vfs/vfscommon"
	"github.com/stretchr/testify/require"
)

type snapshotTestFS struct {
	fs.Fs
	mu      sync.Mutex
	base    string
	updates []string
	gate    func()
}

func (f *snapshotTestFS) ManagedSnapshotBase(context.Context, string) (string, error) {
	if f.base != "" {
		return "base", nil
	}
	return "", nil
}
func (f *snapshotTestFS) OpenManagedSnapshot(context.Context, string) (io.ReadCloser, error) {
	return os.Open(f.base)
}
func (f *snapshotTestFS) Put(ctx context.Context, r io.Reader, src fs.ObjectInfo, opts ...fs.OpenOption) (fs.Object, error) {
	if f.gate != nil {
		f.gate()
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.updates = append(f.updates, string(b))
	f.mu.Unlock()
	o, err := f.Fs.Put(ctx, strings.NewReader(string(b)), src, opts...)
	if err != nil {
		return nil, err
	}
	return &snapshotTestObject{Object: o, f: f}, nil
}

type snapshotTestObject struct {
	fs.Object
	f *snapshotTestFS
}

func (o *snapshotTestObject) Update(ctx context.Context, r io.Reader, src fs.ObjectInfo, opts ...fs.OpenOption) error {
	v, err := o.f.Put(ctx, r, src, opts...)
	if err == nil {
		o.Object = v.(*snapshotTestObject).Object
	}
	return err
}

func journalItem(t *testing.T, base string) (*Cache, *Item, *snapshotTestFS) {
	t.Helper()
	run := fstest.NewRun(t)
	f := &snapshotTestFS{Fs: run.Fremote}
	opt := vfscommon.Opt
	opt.CachePollInterval, opt.WriteBack, opt.HandleCaching = 0, 0, 0
	ctx, cancel := context.WithCancel(context.Background())
	c, err := New(ctx, f, &opt, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.CleanUp()); cancel() })
	require.True(t, c.EnableManagedSync())
	var o fs.Object
	if base != "" {
		f.base = filepath.Join(t.TempDir(), "base")
		require.NoError(t, os.WriteFile(f.base, []byte(base), 0600))
		var err error
		o, err = f.Fs.Put(context.Background(), strings.NewReader(base), object.NewStaticObjectInfo("note.md", time.Now(), int64(len(base)), true, nil, f))
		require.NoError(t, err)
		o = &snapshotTestObject{Object: o, f: f}
	}
	i, _ := c.get("note.md")
	require.NoError(t, i.Open(o))
	t.Cleanup(func() { i.closeManagedJournal() })
	return c, i, f
}

func flushCheckpoint(t *testing.T, i *Item) {
	t.Helper()
	_, err := i.FlushManaged(context.Background(), nil)
	require.NoError(t, err)
}
func readJournal(t *testing.T, i *Item) string {
	t.Helper()
	size, err := i.GetSize()
	require.NoError(t, err)
	b := make([]byte, size)
	n, err := i.ReadAt(b, 0)
	if err != io.EOF {
		require.NoError(t, err)
	}
	require.Equal(t, len(b), n)
	return string(b)
}

func TestLocalCheckpointDoesNotStageAndRetainsEverySave(t *testing.T) {
	_, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("first"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	require.Empty(t, f.updates, "save must not enter full-file hashing/import")
	_, err = i.WriteAt([]byte("later"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	flushCheckpoint(t, i)
	require.Equal(t, "later", readJournal(t, i))
	_, err = i.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "later"}, f.updates)
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointPartialWriteUsesImmutableBaseWithoutDownload(t *testing.T) {
	_, i, f := journalItem(t, "0123456789")
	_, err := i.WriteAt([]byte("XX"), 3)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	require.Empty(t, f.updates)
	require.Equal(t, "012XX56789", readJournal(t, i))
	_, err = i.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"012XX56789"}, f.updates)
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointTruncateExtendDoesNotExposeOldTail(t *testing.T) {
	_, i, f := journalItem(t, "private old content")
	require.NoError(t, i.Truncate(0))
	require.NoError(t, i.Truncate(5))
	_, err := i.WriteAt([]byte("x"), 7)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	want := strings.Repeat("\x00", 7) + "x"
	require.Equal(t, want, readJournal(t, i))
	_, err = i.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{want}, f.updates)
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointCanSaveWhileArchiveIsBlocked(t *testing.T) {
	_, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("first"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.gate = func() { once.Do(func() { close(entered); <-leave }) }
	done := make(chan error, 1)
	go func() { _, err := i.FlushForManagedRename(context.Background(), nil); done <- err }()
	<-entered
	saved := make(chan error, 1)
	go func() {
		_, err := i.WriteAt([]byte("later"), 0)
		if err == nil {
			_, err = i.FlushManaged(context.Background(), nil)
		}
		saved <- err
	}()
	select {
	case err := <-saved:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		close(leave)
		<-done
		t.Fatal("local save waited for background archive")
	}
	close(leave)
	require.NoError(t, <-done)
	require.Equal(t, []string{"first", "later"}, f.updates)
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointSurvivesRestartAndIgnoresUncommittedTail(t *testing.T) {
	c, i, f := journalItem(t, "old base")
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	_, err = i.WriteAt([]byte("later unflushed"), 0)
	require.NoError(t, err)
	// Simulate process termination without another Flush/Close accepting the tail.
	require.NoError(t, i.closeCache(nil))
	i.closeManagedJournal()
	recovered := newItem(c, "note.md")
	require.NoError(t, recovered.Open(nil))
	require.Equal(t, "savedase", readJournal(t, recovered))
	_, err = recovered.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"savedase"}, f.updates)
	require.NoError(t, recovered.Close(nil))
	recovered.closeManagedJournal()
}

func TestLocalCheckpointSyncFailureKeepsPending(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	saved, preparing, _ := c.ManagedLocalSaveStatus()
	require.False(t, saved)
	require.Equal(t, 1, preparing)
	real := c.managedIO.syncFile
	c.managedIO.syncFile = func(*os.File) error { return syscall.ENOSPC }
	_, err = i.FlushManaged(context.Background(), nil)
	require.ErrorIs(t, err, syscall.ENOSPC)
	require.True(t, i.IsDirty())
	require.True(t, c.ManagedPending())
	c.managedIO.syncFile = real
	flushCheckpoint(t, i)
	saved, preparing, _ = c.ManagedLocalSaveStatus()
	require.True(t, saved, "background preparation is independent of local durability")
	require.Equal(t, 1, preparing)
	pending, err := ManagedMetadataPending(c.root, c.metaRoot)
	require.NoError(t, err)
	require.True(t, pending)
	_, err = i.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, i.Close(nil))
}

func TestLocalCheckpointCommittedCorruptionFailsClosed(t *testing.T) {
	c, i, _ := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	p, err := journalPath(c.metaRoot, i.info.Journal)
	require.NoError(t, err)
	f, err := os.OpenFile(p, os.O_RDWR, 0600)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xff}, i.info.JournalEnd-1)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	before, err := os.ReadFile(p)
	require.NoError(t, err)
	_, err = ManagedMetadataPending(c.root, c.metaRoot)
	require.Error(t, err)
	_, err = NewManaged(context.Background(), c.fremote, c.opt, nil, nil)
	require.Error(t, err, "startup must not remove a corrupt accepted checkpoint")
	after, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestLocalCheckpointAckSurvivesRestartWithoutReplayingOldSaves(t *testing.T) {
	c, i, f := journalItem(t, "")
	for _, body := range []string{"first", "later"} {
		_, err := i.WriteAt([]byte(body), 0)
		require.NoError(t, err)
		flushCheckpoint(t, i)
	}
	_, err := i.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, i.closeCache(nil))
	i.closeManagedJournal()
	recovered := newItem(c, "note.md")
	o, err := f.NewObject(context.Background(), "note.md")
	require.NoError(t, err)
	require.NoError(t, recovered.Open(o))
	_, err = recovered.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "later"}, f.updates)
	_, err = recovered.WriteAt([]byte("newer"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, recovered)
	_, err = recovered.FlushForManagedRename(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "later", "newer"}, f.updates)
	require.NoError(t, recovered.Close(nil))
	recovered.closeManagedJournal()
}

func TestLocalCheckpointWorkerProcessesAndRebasesAfterClose(t *testing.T) {
	c, i, f := journalItem(t, "")
	_, err := i.WriteAt([]byte("saved"), 0)
	require.NoError(t, err)
	flushCheckpoint(t, i)
	// Leave a short torn tail, as if the process died during the next write.
	_, err = i.journal.file.Write([]byte("incomplete frame"))
	require.NoError(t, err)
	require.NoError(t, i.closeCache(nil))
	i.closeManagedJournal()
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recovered, err := NewManaged(workerCtx, f, c.opt, nil, nil)
	require.NoError(t, err)
	defer recovered.StopManagedArchives()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.updates) == 1
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, recovered.DrainManagedArchives(context.Background()))
	pending, err := ManagedMetadataPending(c.root, c.metaRoot)
	require.NoError(t, err)
	require.False(t, pending)
	b, err := os.ReadFile(c.toOSPath("note.md"))
	require.NoError(t, err)
	require.Equal(t, "saved", string(b))
}
