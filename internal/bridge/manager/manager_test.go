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
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type fakeRuntime struct {
	mu                             sync.Mutex
	readers, writers, reservations int
	pending                        bool
	paused, unmounted              bool
	done                           chan error
}

func (f *fakeRuntime) Activity() (int, int, int) {
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
func (f *fakeRuntime) Unmount() error               { f.mu.Lock(); f.unmounted = true; f.mu.Unlock(); return nil }
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
	if err = os.Mkdir(mount.Mountpoint, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "settings", "config.json")
	if err = config.Save(file, c); err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{done: make(chan error)}
	workerCtx, cancel := context.WithCancel(context.Background())
	m, err := newManager(Options{ConfigFile: file, CLIExecutable: "", Executable: "/fixture/bridge", ReadyTimeout: 2 * time.Second}, dependencies{domain: filepath.Join(root, "global"), startLease: func(context.Context) (func(), error) { return func() {}, nil }, factory: func(ctx context.Context, spec mountSpec) (*workerServices, error) {
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
	m.spawn = func(context.Context, []string, string) (int, error) {
		go func() {
			err := m.RunWorker(workerCtx, mid)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("worker exited: %v", err)
			}
		}()
		return os.Getpid(), nil
	}
	t.Cleanup(func() { cancel(); _ = m.Close() })
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
