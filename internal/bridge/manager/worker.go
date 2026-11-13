package manager

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
)

type mountRuntime interface {
	Activity() (int, int, int)
	StopActivity() (int, int, int)
	CachePending() bool
	Pause() (func(), error)
	Unmount() error
	Close() error
	Done() <-chan error
	CachePaths() (string, string)
}
type workerServices struct {
	restoreHistory     func(context.Context, model.HistoryRestoreRequest, io.Reader, *store.HistoryRestoreBundle) (model.HistoryRestoreResult, error)
	directoryStatus    func(context.Context) (model.DirectoryIndexStatus, error)
	cacheBytes         func() (int64, bool)
	store              *store.Store
	runtime            mountRuntime
	run                func(context.Context) error
	syncNow            func(context.Context) error
	resolve            func(context.Context, model.ResolveRequest) error
	retry              func(context.Context) error
	refreshConflict    func(context.Context, model.ConflictID) (model.Conflict, error)
	refreshDirectories func(context.Context) error
	resumeCredentials  func(context.Context) error
}
type worker struct {
	historyState model.HistoryStatus
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
	controls     map[uint64]context.CancelFunc
	controlNext  uint64
	controlWG    sync.WaitGroup
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
	if spec.Mount.Archived {
		return ErrArchived
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
		// Offline status also owns this lock while inspecting a persisted Store.
		// Allow that short read to finish, but never replace a live authenticated
		// worker or wait indefinitely for any other owner.
		claimCtx, cancel := context.WithTimeout(ctx, m.options.ReadyTimeout)
		defer cancel()
		var current model.MountStatus
		if probeErr := m.call(claimCtx, id, "/status", nil, &current); probeErr == nil {
			return ErrPending
		} else if errors.Is(probeErr, ErrInstanceChanged) {
			return probeErr
		}
		ok, err = l.TryLockContext(claimCtx, 10*time.Millisecond)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return errors.Join(ErrPending, err)
			}
			return err
		}
		if !ok {
			return ErrPending
		}
	}
	defer l.Unlock()
	// Archive holds this same runtime lock while persisting its marker. Recheck
	// after acquiring it so a direct _worker with an older spec cannot revive it.
	current, err := m.ResolveMount(ctx, string(id))
	if err != nil {
		return err
	}
	if current.Archived {
		return ErrArchived
	}
	w := &worker{manager: m, spec: spec, instance: uuid.NewString(), stopped: make(chan struct{}), controls: map[uint64]context.CancelFunc{}}
	m.mu.Lock()
	m.workers[id] = w
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.workers, id); m.mu.Unlock() }()
	if err = m.validateOwnership(ctx, spec); err != nil {
		w.recordError(err)
		return err
	}
	// Recheck in the worker as well as its spawning parent: direct _worker
	// invocation must not bypass the selected runtime target's safety checks.
	if err = validateMountpoint(spec.Mount.Mountpoint); err != nil {
		w.recordError(err)
		return err
	}
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
	if services.resumeCredentials != nil {
		if err = services.resumeCredentials(ctx); err != nil {
			w.recordError(err)
			_ = services.runtime.Unmount()
			return err
		}
	}
	engineCtx, cancel := context.WithCancel(context.Background())
	w.engineCancel = cancel
	w.engineDone = make(chan struct{})
	historyDone := make(chan struct{})
	go func() { defer close(historyDone); w.runHistory(engineCtx) }()
	go func() {
		defer close(w.engineDone)
		if services.run != nil {
			if err := services.run(engineCtx); err != nil && !errors.Is(err, context.Canceled) {
				w.recordError(err)
			}
		}
	}()
	defer func() { cancel(); w.closeControls(); <-w.engineDone; <-historyDone }()
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
	if err == nil {
		return
	}
	code := errorCode(err)
	w.mu.Lock()
	w.lastError = code
	w.mu.Unlock()
	_ = atomicJSON(filepath.Join(w.spec.Paths.Root, "last-state.json"), map[string]string{"last_error": code})
	w.log(code)
}
func (w *worker) status(ctx context.Context) (model.MountStatus, error) {
	w.mu.Lock()
	services, ready, last, historyState := w.services, w.ready, w.lastError, w.historyState
	w.mu.Unlock()
	out := model.MountStatus{CloudSyncInterval: syncer.BackgroundSyncInterval.String(), SchemaVersion: 1, MountID: w.spec.Mount.ID, Running: ready, CloudState: "starting", LastError: last}
	if services == nil {
		return out, nil
	}
	out, err := storeStatus(ctx, w.spec.Mount.ID, services.store)
	if err != nil {
		return out, err
	}
	out.Running = ready
	out.History = &historyState
	if last != "" {
		out.LastError = last
	}
	if services.runtime.CachePending() {
		out.VFSPending++
	}
	_, writers, _ := services.runtime.Activity()
	out.LocalSaved = out.VFSPending == 0 && writers == 0
	if local, ok := services.runtime.(interface{ LocalSaveStatus() (bool, int, []string) }); ok {
		var preparing int
		var failed []string
		out.LocalSaved, preparing, failed = local.LocalSaveStatus()
		out.Queued += preparing
		if preparing > 0 && out.CloudState == "synced" {
			out.CloudState = "queued"
		}
		for _, name := range failed {
			out.Issues = append(out.Issues, model.SyncIssue{Name: name, Code: "local_archive_failed", State: model.StatusBlocked, LocalSaved: out.LocalSaved})
		}
		if len(failed) > 0 {
			out.LastError = "local_archive_failed"
			out.CloudState = "blocked"
		}
	}
	if local, ok := services.runtime.(interface{ LocalSaveFailures() []string }); ok {
		failed := local.LocalSaveFailures()
		for _, name := range failed {
			out.Issues = append(out.Issues, model.SyncIssue{Name: name, Code: "local_save_failed", State: model.StatusBlocked, LocalSaved: false})
		}
		if len(failed) > 0 {
			out.LocalSaved = false
			out.LastError = "local_save_failed"
			out.CloudState = "blocked"
		}
	}
	if services.cacheBytes != nil {
		if bytes, ok := services.cacheBytes(); ok {
			out.CacheBytes = &bytes
		}
	}
	if services.directoryStatus != nil {
		if metadata, metadataErr := services.directoryStatus(ctx); metadataErr == nil {
			out.DirectoryIndex = &metadata
		}
	}
	return out, nil
}
func (w *worker) stop(ctx context.Context, wait time.Duration) error {
	return w.stopPolicy(ctx, wait, false)
}
func (w *worker) stopPolicy(ctx context.Context, wait time.Duration, preserve bool) error {
	w.mu.Lock()
	if w.stopping || !w.ready || w.services == nil {
		w.mu.Unlock()
		return ErrPending
	}
	w.stopping = true
	services := w.services
	if preserve {
		for _, cancel := range w.controls {
			cancel()
		}
	}
	w.mu.Unlock()
	success := false
	keepPaused := false
	defer func() { w.mu.Lock(); w.stopping = false; w.mu.Unlock() }()
	resume, err := services.runtime.Pause()
	if err != nil {
		return ErrPending
	}
	defer func() {
		if !success && !keepPaused {
			resume()
		}
	}()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	stopDrain := func() {}
	draining := false
	defer func() { stopDrain() }()
	for {
		status, err := w.status(ctx)
		if err != nil {
			return err
		}
		readers, writers, reservations := services.runtime.StopActivity()
		publications, pubErr := services.store.PendingPublications(ctx)
		names, nsErr := services.store.NamespaceActions(ctx)
		creates, crErr := services.store.PendingCreates(ctx)
		if pubErr != nil || nsErr != nil || crErr != nil {
			return ErrPending
		}
		if !preserve && wait > 0 && !draining && services.syncNow != nil && (status.Queued > 0 || status.Inflight > 0 || len(publications) > 0 || len(names) > 0 || len(creates) > 0) {
			stopDrain = startStopSyncDrain(ctx, services.syncNow)
			draining = true
		}
		w.mu.Lock()
		controls := len(w.controls)
		w.mu.Unlock()
		if controls == 0 && readers == 0 && writers == 0 && reservations == 0 && status.VFSPending == 0 && (preserve || (status.Queued == 0 && status.Inflight == 0 && status.Conflicts == 0 && len(publications) == 0 && len(names) == 0 && len(creates) == 0)) {
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
	stopDrain()
	w.engineCancel()
	<-w.engineDone
	if err := services.runtime.Unmount(); err != nil {
		keepPaused = true
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
	return m.startWithDesktopIntent(ctx, id, false)
}

// startAdmitted requires the worker-start lease and desktop operation lock.
func (m *Manager) startAdmitted(readyCtx context.Context, id model.MountID) (model.MountStatus, error) {
	spec, err := m.spec(readyCtx, id)
	if err != nil {
		return model.MountStatus{}, err
	}
	if spec.Mount.Archived {
		return model.MountStatus{}, ErrArchived
	}
	if err = m.validateOwnership(readyCtx, spec); err != nil {
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
	if m.options.GitExecutable != "" {
		args = append(args, "--git-executable", m.options.GitExecutable)
	}
	args = append(args, "_worker", "--mount-id", string(id))
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	spawned := false
	var state model.MountStatus
	for {
		if spawned {
			state, err = m.startupStatus(readyCtx, spec)
		} else {
			state, err = m.Status(readyCtx, id)
		}
		if err == nil && state.Running {
			return state, nil
		}
		if spawned && err == nil && state.LastError != "" {
			return state, errorFromCode(state.LastError)
		}
		if !spawned {
			lock, lockErr := privateLock(m.runtimeLockPath(id))
			if lockErr != nil {
				return state, lockErr
			}
			ok, lockErr := lock.TryLock()
			if lockErr != nil {
				return state, lockErr
			}
			if ok {
				if err = validateMountpoint(spec.Mount.Mountpoint); err != nil {
					_ = lock.Unlock()
					return state, err
				}
				// A free OS ownership lock proves that an old descriptor cannot belong to
				// a managed worker. Never signal or kill the descriptor's recorded PID.
				for _, name := range []string{"ipc.json", "ipc.key"} {
					path := filepath.Join(spec.Paths.Root, name)
					if err = platform.CheckNoSymlinks(path); err != nil {
						_ = lock.Unlock()
						return state, err
					}
					if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
						_ = lock.Unlock()
						return state, removeErr
					}
				}
				err = atomicJSON(filepath.Join(spec.Paths.Root, "last-state.json"), map[string]string{"last_error": ""})
				_ = lock.Unlock()
				if err != nil {
					return state, err
				}
				if _, err = m.spawn(readyCtx, args, log); err != nil {
					return state, err
				}
				spawned = true
			} else if errors.Is(err, ErrInstanceChanged) {
				return state, err
			}
		}
		select {
		case <-readyCtx.Done():
			return state, readyCtx.Err()
		case <-tick.C:
		}
	}
}

func (m *Manager) Stop(ctx context.Context, id model.MountID, wait time.Duration) error {
	if wait < 0 {
		return ErrConfig
	}
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	disabled := false
	if _, err = m.desktopIntent(ctx, id, &disabled, false); err != nil {
		return err
	}
	return m.stopAdmitted(ctx, id, wait)
}

// stopAdmitted performs a safe maintenance stop without changing user intent.
// Its caller already holds lifecycle admission, which must not be re-entered.
func (m *Manager) stopAdmitted(ctx context.Context, id model.MountID, wait time.Duration) error {
	var out struct{}
	err := m.call(ctx, id, "/stop", map[string]int64{"wait_ns": int64(wait)}, &out)
	if err != nil && !errors.Is(err, ErrUnavailable) && !os.IsNotExist(err) {
		return err
	}
	finishCtx, cancel := context.WithTimeout(ctx, wait+time.Second)
	defer cancel()
	if err = m.waitOffline(finishCtx, id); err != nil {
		return errors.Join(ErrPending, err)
	}
	status, err := m.Status(ctx, id)
	if err != nil {
		return err
	}
	if status.Running || status.Queued+status.Inflight+status.Conflicts+status.VFSPending > 0 {
		return ErrPending
	}
	return nil
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
	return prepareMountpointForOS(path, runtime.GOOS, false)
}

func (w *worker) beginControl(parent context.Context) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ready || w.stopping {
		return nil, nil, ErrPending
	}
	ctx, cancel := context.WithCancel(parent)
	w.controlNext++
	id := w.controlNext
	if w.controls == nil {
		w.controls = map[uint64]context.CancelFunc{}
	}
	w.controls[id] = cancel
	w.controlWG.Add(1)
	var once sync.Once
	return ctx, func() {
		once.Do(func() { cancel(); w.mu.Lock(); delete(w.controls, id); w.mu.Unlock(); w.controlWG.Done() })
	}, nil
}
func (w *worker) closeControls() {
	w.mu.Lock()
	w.stopping = true
	w.ready = false
	for _, cancel := range w.controls {
		cancel()
	}
	w.mu.Unlock()
	w.controlWG.Wait()
}
