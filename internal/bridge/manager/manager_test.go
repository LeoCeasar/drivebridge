package manager

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type fakeRuntime struct {
	mu                             sync.Mutex
	readers, writers, reservations int
	unmountErr                     error
	directoryReaders               int
	pending                        bool
	paused, unmounted              bool
	done                           chan error
}

func (f *fakeRuntime) Activity() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readers + f.directoryReaders, f.writers, f.reservations
}
func (f *fakeRuntime) StopActivity() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readers, f.writers, f.reservations
}
func (f *fakeRuntime) CachePending() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.pending }
func (f *fakeRuntime) Pause() (func(), error) {
	f.mu.Lock()
	f.paused = true
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.paused = false; f.mu.Unlock() }, nil
}
func (f *fakeRuntime) Unmount() error {
	f.mu.Lock()
	f.unmounted = true
	err := f.unmountErr
	f.mu.Unlock()
	return err
}
func (f *fakeRuntime) Close() error                 { return nil }
func (f *fakeRuntime) Done() <-chan error           { return f.done }
func (f *fakeRuntime) CachePaths() (string, string) { return "", "" }
func fixture(t *testing.T, gate chan struct{}) (*Manager, model.MountID, *fakeRuntime, context.CancelFunc) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pid := model.ProfileID(uuid.NewString())
	rid := model.RemoteID(uuid.NewString())
	mid := model.MountID(uuid.NewString())
	c := config.Default()
	c.StateDir = filepath.Join(root, "state")
	c.CacheDir = filepath.Join(root, "cache")
	c.Profiles = []model.Profile{{ID: pid, Name: "work", Brand: "feishu", AuthRef: "auth/identities/" + uuid.NewString() + "/lark", IdentityHash: strings.Repeat("a", 64)}}
	c.Remotes = []model.Remote{{ID: rid, Name: "team", ProfileID: pid, FolderToken: "fixture_folder", NewMDPolicy: "docx"}}
	mount := config.DefaultMount()
	mount.ID = mid
	mount.Name = "team"
	mount.RemoteID = rid
	mount.Mountpoint = filepath.Join(root, "mountpoint")
	c.Mounts = []model.Mount{mount}
	if err = validateMountpoint(mount.Mountpoint); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "settings", "config.json")
	if err = config.Save(file, c); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{done: make(chan error)}
	workerCtx, cancel := context.WithCancel(context.Background())
	m, err := newManager(Options{ConfigFile: file, CLIExecutable: "", Executable: "/fixture/bridge", ReadyTimeout: 2 * time.Second}, dependencies{domain: filepath.Join(root, "global"), startLease: func(ctx context.Context) (func(), error) {
		return fixtureLease(ctx, filepath.Join(root, "global", "test-admission.lock"), true)
	}, lifecycleLease: func(ctx context.Context) (func(), error) {
		return fixtureLease(ctx, filepath.Join(root, "global", "test-admission.lock"), false)
	}, factory: func(ctx context.Context, spec mountSpec) (*workerServices, error) {
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		s, err := store.Open(spec.Paths.Root)
		if err != nil {
			return nil, err
		}
		if err = s.BindScope(ctx, store.Scope{MountID: spec.Mount.ID, ProfileID: spec.Profile.ID, RemoteID: spec.Remote.ID, FolderToken: spec.Remote.FolderToken, IdentityHash: strings.Repeat("a", 64)}); err != nil {
			s.Close()
			return nil, err
		}
		return &workerServices{store: s, runtime: runtime, run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	m.spawn = func(context.Context, []string, string) (int, error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := m.RunWorker(workerCtx, mid)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("worker exited: %v", err)
			}
		}()
		return os.Getpid(), nil
	}
	t.Cleanup(func() { cancel(); _ = m.Close(); workers.Wait() })
	return m, mid, runtime, cancel
}
func TestReadyRequiresMountAndIPC(t *testing.T) {
	gate := make(chan struct{})
	m, id, _, _ := fixture(t, gate)
	done := make(chan error, 1)
	go func() { _, err := m.Start(context.Background(), id, true); done <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.waitEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), id)
	if err != nil || status.Running {
		t.Fatalf("reported ready before kernel mount: %+v %v", status, err)
	}
	select {
	case err := <-done:
		t.Fatal("start returned before mount", err)
	default:
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	status, err = m.Status(context.Background(), id)
	if err != nil || !status.Running {
		t.Fatalf("ready missing: %+v %v", status, err)
	}
}
func TestStopTimeoutKeepsWorkerAndPending(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	e, err := w.services.store.EnsureEntry(context.Background(), "draft.bin", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.services.store.Stage(context.Background(), e.ID, nil, strings.NewReader("do not lose")); err != nil {
		t.Fatal(err)
	}
	if err = m.Stop(context.Background(), id, 25*time.Millisecond); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), id)
	if err != nil || !status.Running || status.Queued != 1 {
		t.Fatalf("unsafe stop: %+v %v", status, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.unmounted || runtime.paused {
		t.Fatal("failed stop unmounted or left paused")
	}
}
func TestIPCRejectsUnauthorizedAndPIDReuse(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	ep, _, err := m.readEndpoint(id)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", ep.Address+"/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatal(res.StatusCode)
	}
	ep.PID++
	if err = m.writeEndpoint(id, ep); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Status(context.Background(), id); !errors.Is(err, ErrInstanceChanged) {
		t.Fatalf("reused PID endpoint accepted: %v", err)
	}
}
func TestCacheCleanSkipsReferencedData(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	e, _ := w.services.store.EnsureEntry(context.Background(), "pending.bin", model.KindFile)
	entry, _, err := w.services.store.Stage(context.Background(), e.ID, nil, strings.NewReader("pending safe"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.CleanCache(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	reader, err := w.services.store.ReadBlob(context.Background(), entry.Local)
	if err != nil {
		t.Fatal("cache clean deleted pending data", err)
	}
	reader.Close()
}
func TestRecoveryStopPreservesPendingAndReportsOfflineQueue(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	e, _ := w.services.store.EnsureEntry(context.Background(), "pending.md", model.KindDocx)
	if _, _, err := w.services.store.Stage(context.Background(), e.ID, nil, strings.NewReader("preserved")); err != nil {
		t.Fatal(err)
	}
	if err := m.stopForRecovery(context.Background(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), id)
	if err != nil || status.Running || status.Queued != 1 {
		t.Fatalf("lost pending status after recovery stop: %+v %v", status, err)
	}
}
func TestUnstartedMountQueriesStayEmptyWithoutCreatingStore(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	status, err := m.Status(context.Background(), id)
	if err != nil || status.Running || status.Queued != 0 {
		t.Fatal(status, err)
	}
	if list, err := m.Conflicts(context.Background(), id); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	if list, err := m.UploadStatus(context.Background(), id); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	if value, err := m.CacheStatus(context.Background(), id); err != nil || value.BlobBytes != 0 {
		t.Fatal(value, err)
	}
	spec, _ := m.spec(context.Background(), id)
	if _, err := os.Stat(filepath.Join(spec.Paths.Root, "store.db")); !os.IsNotExist(err) {
		t.Fatal("read-only empty query created a database", err)
	}
}
func fixtureReference(t *testing.T, m *Manager) []auth.Reference {
	t.Helper()
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles[0]
	return []auth.Reference{{ConfigID: hashPath(m.ConfigFile()), ConfigFile: m.ConfigFile(), ProfileID: p.ID, AuthRef: p.AuthRef, IdentityHash: p.IdentityHash}}
}
func TestAuthGuardRefusesLiveMountAndRecoveryKeepsPendingFrozen(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	refs := fixtureReference(t, m)
	if release, err := m.Acquire(context.Background(), "app", refs); !errors.Is(err, auth.ErrReferencesBusy) {
		if release != nil {
			release()
		}
		t.Fatal("live mount did not block logout", err)
	}
	w := m.localWorker(id)
	e, _ := w.services.store.EnsureEntry(context.Background(), "pending.bin", model.KindFile)
	if _, _, err := w.services.store.Stage(context.Background(), e.ID, nil, strings.NewReader("pending preserved")); err != nil {
		t.Fatal(err)
	}
	release, err := m.AcquireRecovery(context.Background(), "app", refs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UploadStatus(context.Background(), id); !errors.Is(err, ErrPending) {
		release()
		t.Fatal("offline pending state was not frozen", err)
	}
	release()
	jobs, err := m.UploadStatus(context.Background(), id)
	if err != nil || len(jobs) != 1 {
		t.Fatal("recovery discarded pending input", jobs, err)
	}
	if release, err = m.Acquire(context.Background(), "app", refs); !errors.Is(err, auth.ErrReferencesBusy) {
		if release != nil {
			release()
		}
		t.Fatal("pending queue did not block logout", err)
	}
}
func TestHistoricalStateRootCannotHidePendingFromAuthGuard(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	e, _ := w.services.store.EnsureEntry(context.Background(), "pending.bin", model.KindFile)
	if _, _, err := w.services.store.Stage(context.Background(), e.ID, nil, strings.NewReader("old-root-pending")); err != nil {
		t.Fatal(err)
	}
	if err := m.stopForRecovery(context.Background(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	refs := fixtureReference(t, m)
	c, _ := m.LoadConfig(context.Background())
	c.StateDir = filepath.Join(filepath.Dir(c.StateDir), "other-state")
	if err := config.Save(m.ConfigFile(), c); err != nil {
		t.Fatal(err)
	}
	if release, err := m.Acquire(context.Background(), "app", refs); !errors.Is(err, auth.ErrReferencesBusy) {
		if release != nil {
			release()
		}
		t.Fatal("StateDir override hid durable queue", err)
	}
	if _, err := m.Start(context.Background(), id, true); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("moved StateDir adopted without migration", err)
	}
}
func TestStatusIncludesNamespaceCloudWork(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	if _, err := s.ObserveEntry(context.Background(), store.Observation{Remote: "", Kind: model.KindFolder, Token: "fixture_folder"}); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareNamespace(context.Background(), model.NamespaceOp{Kind: "mkdir", Dst: "new-folder"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(context.Background(), op.ID); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), id)
	if err != nil || status.Queued+status.Inflight == 0 || status.CloudState == "synced" {
		t.Fatal("unpublished namespace reported synced", status, err)
	}
}
func TestStopDoesNotUnmountDuringMutableIPC(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	w.mu.Lock()
	w.services.retry = func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	w.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- m.Retry(context.Background(), id) }()
	<-entered
	if err := m.Stop(context.Background(), id, 30*time.Millisecond); !errors.Is(err, ErrPending) {
		t.Fatal("stop crossed active mutable IPC", err)
	}
	runtime.mu.Lock()
	unmounted := runtime.unmounted
	runtime.mu.Unlock()
	if unmounted {
		t.Fatal("unmounted before request finished")
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestRecoveryJoinsControlCancellationBeforeClosingStore(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	e, err := w.services.store.EnsureEntry(context.Background(), "late-journal.bin", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	saved := make(chan error, 1)
	w.mu.Lock()
	w.services.retry = func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		_, _, err := w.services.store.Stage(context.Background(), e.ID, nil, strings.NewReader("late durable state"))
		saved <- err
		return ctx.Err()
	}
	w.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- m.Retry(context.Background(), id) }()
	<-entered
	if err = m.stopForRecovery(context.Background(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err = <-saved; err != nil {
		t.Fatal("store closed before journal cancellation completed", err)
	}
	_ = <-done
	jobs, err := m.UploadStatus(context.Background(), id)
	if err != nil || len(jobs) != 1 {
		t.Fatal("recovery discarded late durable input", jobs, err)
	}
}
func TestQuiescenceStopsMountAndExcludesWorkerStarts(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	called := false
	err := m.WithQuiescence(context.Background(), func(ctx context.Context) error {
		called = true
		if m.localWorker(id) != nil {
			t.Fatal("callback before worker stopped")
		}
		blocked, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if release, err := m.startLease(blocked); !errors.Is(err, context.DeadlineExceeded) {
			if release != nil {
				release()
			}
			t.Fatal("quiescence did not exclude starts", err)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatal(err)
	}
}
func TestQuiescenceDoesNotCallMigrationWhileWriterIsOpen(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.writers = 1
	runtime.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	called := false
	err := m.WithQuiescence(ctx, func(context.Context) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("migration crossed open writer", err)
	}
	runtime.mu.Lock()
	unmounted := runtime.unmounted
	runtime.mu.Unlock()
	if unmounted {
		t.Fatal("migration unmounted open writer")
	}
}

func fixtureLease(ctx context.Context, path string, shared bool) (func(), error) {
	l, err := privateLock(path)
	if err != nil {
		return nil, err
	}
	var ok bool
	if shared {
		ok, err = l.TryRLockContext(ctx, time.Millisecond)
	} else {
		ok, err = l.TryLockContext(ctx, time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrPending
	}
	var once sync.Once
	return func() { once.Do(func() { _ = l.Unlock() }) }, nil
}

func TestLifecycleExcludesPublicConfigWriters(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	release, err := m.lifecycleLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err = m.AddProfile(ctx, "excluded", "feishu"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("config writer bypassed lifecycle admission", err)
	}
	c, err := m.LoadConfig(context.Background())
	if err != nil || len(c.Profiles) != 1 {
		t.Fatal(c.Profiles, err)
	}
}

func TestHistoricalEndpointDoesNotDependOnCurrentConfig(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.updateConfig(context.Background(), func(c *config.Config) error {
		c.Mounts = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = m.readEndpoint(id); !errors.Is(err, ErrConfig) {
		t.Fatal(err)
	}
	ep, key, err := readEndpointAt(spec.Paths.Root, id, m.options.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	var status model.MountStatus
	if err = callEndpoint(context.Background(), ep, key, "/status", nil, &status); err != nil || !status.Running {
		t.Fatal("historical worker unreachable", status, err)
	}
	if _, _, err = readEndpointAt(spec.Paths.Root, id, m.options.ConfigFile+".other"); !errors.Is(err, ErrInstanceChanged) {
		t.Fatal("historical endpoint accepted wrong config", err)
	}
}

func TestMountWorkerIsolation(t *testing.T) {
	m, first, _, _ := fixture(t, nil)
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	other := c.Mounts[0]
	other.ID = model.MountID(uuid.NewString())
	other.Name = "second"
	other.Mountpoint = filepath.Join(filepath.Dir(other.Mountpoint), "second-mountpoint")
	if err = validateMountpoint(other.Mountpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = m.AddMount(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	factory := m.factory
	m.factory = func(ctx context.Context, spec mountSpec) (*workerServices, error) {
		svc, err := factory(ctx, spec)
		if err == nil && spec.Mount.ID == other.ID {
			svc.runtime = &fakeRuntime{done: make(chan error)}
		}
		return svc, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.spawn = func(_ context.Context, args []string, _ string) (int, error) {
		id := model.MountID(args[len(args)-1])
		go func() { _ = m.RunWorker(ctx, id) }()
		return os.Getpid(), nil
	}
	for _, id := range []model.MountID{first, other.ID} {
		if _, err = m.Start(context.Background(), id, true); err != nil {
			t.Fatal(err)
		}
	}
	a, b := m.localWorker(first), m.localWorker(other.ID)
	if a == nil || b == nil || a == b || a.services.store == b.services.store || a.spec.Paths.Root == b.spec.Paths.Root {
		t.Fatal("mount workers share mutable state")
	}
	if err = m.Stop(context.Background(), first, time.Second); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(context.Background(), other.ID)
	if err != nil || !status.Running {
		t.Fatal("stopping first affected second", status, err)
	}
	if err = m.Stop(context.Background(), other.ID, time.Second); err != nil {
		t.Fatal(err)
	}
}
