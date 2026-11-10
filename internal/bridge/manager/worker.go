package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type mountRuntime interface {
	Activity() (int, int, int)
	CachePending() bool
	Pause() (func(), error)
	Unmount() error
	Close() error
	Done() <-chan error
	CachePaths() (string, string)
}
type workerServices struct {
	store   *store.Store
	runtime mountRuntime
	run     func(context.Context) error
	resolve func(context.Context, model.ResolveRequest) error
	retry   func(context.Context) error
}
type worker struct {
	manager      *Manager
	spec         mountSpec
	instance     string
	mu           sync.Mutex
	services     *workerServices
	ready        bool
	lastError    string
	stopping     bool
	stopped      chan struct{}
	stopOnce     sync.Once
	engineCancel context.CancelFunc
	engineDone   chan struct{}
}

func (m *Manager) localWorker(id model.MountID) *worker {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[id]
}
func (m *Manager) RunWorker(ctx context.Context, id model.MountID) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	spec, err := m.spec(ctx, id)
	if err != nil {
		return err
	}
	l, err := privateLock(m.runtimeLockPath(id))
	if err != nil {
		return err
	}
	ok, err := l.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return ErrPending
	}
	defer l.Unlock()
	w := &worker{manager: m, spec: spec, instance: uuid.NewString(), stopped: make(chan struct{})}
	m.mu.Lock()
	m.workers[id] = w
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.workers, id); m.mu.Unlock() }()
	if err = m.recordOwner(spec); err != nil {
		return err
	}
	server, err := m.serveWorker(w)
	if err != nil {
		return err
	}
	defer server()
	services, err := m.factory(ctx, spec)
	if err != nil {
		w.recordError(err)
		return err
	}
	w.mu.Lock()
	w.services = services
	w.mu.Unlock()
	defer func() { _ = services.runtime.Close(); _ = services.store.Close() }()
	data, meta := services.runtime.CachePaths()
	if err = atomicJSON(filepath.Join(spec.Paths.Root, "cache-manifest.json"), cacheManifest{SchemaVersion: 1, MountID: id, DataRoot: data, MetaRoot: meta}); err != nil {
		w.recordError(err)
		_ = services.runtime.Unmount()
		return err
	}
	engineCtx, cancel := context.WithCancel(context.Background())
	w.engineCancel = cancel
	w.engineDone = make(chan struct{})
	go func() {
		defer close(w.engineDone)
		if services.run != nil {
			if err := services.run(engineCtx); err != nil && !errors.Is(err, context.Canceled) {
				w.recordError(err)
			}
		}
	}()
	defer func() { cancel(); <-w.engineDone }()
	w.mu.Lock()
	w.ready = true
	w.mu.Unlock()
	release()
	release = nil
	w.log("ready")
	select {
	case <-ctx.Done():
		w.log("worker_interrupted")
		_ = services.runtime.Unmount()
		return ctx.Err()
	case <-w.stopped:
		return nil
	case err = <-services.runtime.Done():
		w.recordError(err)
		return err
	}
}
func (w *worker) recordError(err error) {
	code := errorCode(err)
	w.mu.Lock()
	w.lastError = code
	w.mu.Unlock()
	_ = atomicJSON(filepath.Join(w.spec.Paths.Root, "last-state.json"), map[string]string{"last_error": code})
	w.log(code)
}
func (w *worker) status(ctx context.Context) (model.MountStatus, error) {
	w.mu.Lock()
	services, ready, last := w.services, w.ready, w.lastError
	w.mu.Unlock()
	out := model.MountStatus{SchemaVersion: 1, MountID: w.spec.Mount.ID, Running: ready, CloudState: "starting", LastError: last}
	if services == nil {
		return out, nil
	}
	out, err := storeStatus(ctx, w.spec.Mount.ID, services.store)
	if err != nil {
		return out, err
	}
	out.Running = ready
	out.LastError = last
	if services.runtime.CachePending() {
		out.VFSPending++
	}
	_, writers, _ := services.runtime.Activity()
	out.LocalSaved = out.VFSPending == 0 && writers == 0
	return out, nil
}
func (w *worker) stop(ctx context.Context, wait time.Duration) error {
	w.mu.Lock()
	if w.stopping || !w.ready || w.services == nil {
		w.mu.Unlock()
		return ErrPending
	}
	w.stopping = true
	services := w.services
	w.mu.Unlock()
	success := false
	defer func() { w.mu.Lock(); w.stopping = false; w.mu.Unlock() }()
	resume, err := services.runtime.Pause()
	if err != nil {
		return ErrPending
	}
	defer func() {
		if !success {
			resume()
		}
	}()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err := w.status(ctx)
		if err != nil {
			return err
		}
		readers, writers, reservations := services.runtime.Activity()
		publications, pubErr := services.store.PendingPublications(ctx)
		names, nsErr := services.store.NamespaceActions(ctx)
		creates, crErr := services.store.PendingCreates(ctx)
		if pubErr != nil || nsErr != nil || crErr != nil {
			return ErrPending
		}
		if readers == 0 && writers == 0 && reservations == 0 && status.VFSPending == 0 && status.Queued == 0 && status.Inflight == 0 && status.Conflicts == 0 && len(publications) == 0 && len(names) == 0 && len(creates) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ErrPending
		case <-deadline.C:
			return ErrPending
		case <-tick.C:
		}
	}
	w.engineCancel()
	<-w.engineDone
	if err := services.runtime.Unmount(); err != nil {
		w.recordError(err)
		return err
	}
	success = true
	w.mu.Lock()
	w.ready = false
	w.mu.Unlock()
	w.log("stopped")
	w.stopOnce.Do(func() { close(w.stopped) })
	return nil
}
func (m *Manager) Start(ctx context.Context, id model.MountID, background bool) (model.MountStatus, error) {
	release, err := m.startLease(ctx)
	if err != nil {
		return model.MountStatus{}, err
	}
	defer release()
	spec, err := m.spec(ctx, id)
	if err != nil {
		return model.MountStatus{}, err
	}
	if state, err := m.Status(ctx, id); err == nil && state.Running {
		return state, nil
	}
	if err = validateMountpoint(spec.Mount.Mountpoint); err != nil {
		return model.MountStatus{}, err
	}
	log := filepath.Join(spec.Paths.Logs, "worker.stderr.log")
	cli := m.options.CLIExecutable
	if cli != "" {
		cli, err = ResolveCLI(cli, m.options.Executable)
		if err != nil {
			return model.MountStatus{}, err
		}
	}
	args := []string{"feishu", "--bridge-config", m.options.ConfigFile}
	if cli != "" {
		args = append(args, "--lark-cli", cli)
	}
	args = append(args, "_worker", "--mount-id", string(id))
	if _, err = m.spawn(ctx, args, log); err != nil {
		return model.MountStatus{}, err
	}
	deadline := time.NewTimer(m.options.ReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := m.Status(ctx, id)
		if err == nil && state.Running {
			return state, nil
		}
		if errors.Is(err, ErrInstanceChanged) {
			return state, err
		}
		if err == nil && state.LastError != "" {
			return state, errorFromCode(state.LastError)
		}
		select {
		case <-ctx.Done():
			return state, ctx.Err()
		case <-deadline.C:
			return state, ErrReadyTimeout
		case <-ticker.C:
		}
	}
}
func (m *Manager) Stop(ctx context.Context, id model.MountID, wait time.Duration) error {
	if wait < 0 {
		return ErrConfig
	}
	var out struct{}
	return m.call(ctx, id, "/stop", map[string]int64{"wait_ns": int64(wait)}, &out)
}
func (m *Manager) waitEndpoint(ctx context.Context, id model.MountID) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, _, err := m.readEndpoint(id); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func validateMountpoint(path string) error {
	if !filepath.IsAbs(path) && !isDrive(path) {
		return ErrConfig
	}
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return ErrConfig
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return ErrPending
		}
		return nil
	}
	if os.IsNotExist(err) {
		if isDrive(path) {
			return nil
		}
		return os.MkdirAll(path, 0700)
	}
	return err
}
