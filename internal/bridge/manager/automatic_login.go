package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ownedlogin"
)

// automaticReconciledKind is the Flow.Kind of a workflow whose interrupted
// authorization had already completed: there is no link to open, the workflow
// continues straight to verification, owned login and mount.
const automaticReconciledKind = "reconciled"

// AutomaticLoginView uses the first official flow as a stable tracking key.
// Flow always describes the current authorization phase, which may change after
// application registration. Querying this view never launches a browser or mount.
type AutomaticLoginView struct {
	FlowID     string          `json:"flow_id"`
	ProfileID  model.ProfileID `json:"profile_id"`
	State      string          `json:"state"`
	Error      string          `json:"error,omitempty"`
	Flow       auth.AuthFlow   `json:"flow"`
	MountID    model.MountID   `json:"mount_id,omitempty"`
	Mountpoint string          `json:"mountpoint,omitempty"`
}

type automaticAuth interface {
	auth.FlowStarter
	Complete(context.Context, string) (auth.Identity, error)
	Cancel(context.Context, string) error
}
type automaticLoginDependencies struct {
	auth               func(context.Context) (automaticAuth, error)
	open               func(context.Context, model.ProfileID) (DiscoveryView, error)
	view               func(context.Context, model.ProfileID) (DiscoveryView, error)
	status             func(context.Context, model.MountID) (model.MountStatus, error)
	interval, deadline time.Duration
}
type automaticLogin struct {
	view         AutomaticLoginView
	initial      auth.AuthFlow
	initialErr   error
	initialReady chan struct{}
	finished     bool
	cancel       context.CancelFunc
}

func (m *Manager) automaticDependencies() automaticLoginDependencies {
	if m.automaticDeps != nil {
		return *m.automaticDeps
	}
	return automaticLoginDependencies{
		auth: func(ctx context.Context) (automaticAuth, error) { return m.Auth(ctx) },
		open: func(ctx context.Context, id model.ProfileID) (DiscoveryView, error) {
			return m.StartDiscoveryLogin(ctx, id, "")
		},
		view: func(ctx context.Context, id model.ProfileID) (DiscoveryView, error) {
			p, err := m.ResolveProfile(ctx, string(id))
			if err != nil {
				return DiscoveryView{}, err
			}
			return m.discoveryView(ctx, p)
		},
		status: m.Status, interval: time.Second, deadline: 25 * time.Minute,
	}
}

// StartAutomaticLogin accepts a lifecycle-owned workflow. Cancellation of the
// initiating HTTP request only stops that caller waiting for its first URL.
func (m *Manager) StartAutomaticLogin(ctx context.Context, id model.ProfileID, recovery bool) (auth.AuthFlow, error) {
	if err := ctx.Err(); err != nil {
		return auth.AuthFlow{}, err
	}
	m.automaticMu.Lock()
	if m.automaticClosing || m.ctx.Err() != nil {
		m.automaticMu.Unlock()
		return auth.AuthFlow{}, ErrUnavailable
	}
	if m.automaticProfiles == nil {
		m.automaticProfiles = map[model.ProfileID]*automaticLogin{}
		m.automaticFlows = map[string]*automaticLogin{}
	}
	task := m.automaticProfiles[id]
	if task == nil || task.finished {
		deps := m.automaticDependencies()
		deadline := deps.deadline
		if deadline <= 0 {
			deadline = 25 * time.Minute
		}
		lifetime, cancel := context.WithTimeout(m.ctx, deadline)
		task = &automaticLogin{view: AutomaticLoginView{ProfileID: id, State: "waiting_authorization"}, initialReady: make(chan struct{}), cancel: cancel}
		m.automaticProfiles[id] = task
		// Admission and Add share the same mutex as Close's closing flag.
		m.automaticWG.Add(1)
		go m.runAutomaticLogin(lifetime, task, recovery, deps)
	}
	m.automaticMu.Unlock()
	select {
	case <-ctx.Done():
		return auth.AuthFlow{}, ctx.Err()
	case <-task.initialReady:
	}
	m.automaticMu.Lock()
	defer m.automaticMu.Unlock()
	return task.initial, task.initialErr
}

func (m *Manager) AutomaticLoginStatus(ctx context.Context, flowID string) (AutomaticLoginView, error) {
	if err := ctx.Err(); err != nil {
		return AutomaticLoginView{}, err
	}
	m.automaticMu.Lock()
	defer m.automaticMu.Unlock()
	task := m.automaticFlows[flowID]
	if task == nil {
		return AutomaticLoginView{}, os.ErrNotExist
	}
	return task.view, nil
}
func (m *Manager) updateAutomatic(task *automaticLogin, update func(*AutomaticLoginView)) {
	m.automaticMu.Lock()
	defer m.automaticMu.Unlock()
	update(&task.view)
}
func automaticError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, auth.ErrFlowExpired):
		return "expired"
	case errors.Is(err, ownedlogin.ErrRuntimeUnavailable):
		return "needs_runtime"
	case errors.Is(err, ownedlogin.ErrUnavailable):
		return "unsupported"
	case errors.Is(err, ErrDriver):
		return "driver_unavailable"
	case errors.Is(err, auth.ErrReconcileFailed) && !errors.Is(err, auth.ErrBusy) && !errors.Is(err, auth.ErrReferencesBusy) && !errors.Is(err, model.ErrIdentityChanged):
		return "auth_reconcile_failed"
	default:
		return errorCode(err)
	}
}
func (m *Manager) runAutomaticLogin(ctx context.Context, task *automaticLogin, recovery bool, deps automaticLoginDependencies) {
	defer m.automaticWG.Done()
	defer task.cancel()
	var result error
	failureCode := ""
	initialSet := false
	defer func() {
		m.automaticMu.Lock()
		defer m.automaticMu.Unlock()
		task.finished = true
		if result != nil {
			task.view.State = "failed"
			task.view.Error = failureCode
			if task.view.Error == "" {
				task.view.Error = automaticError(result)
			}
		}
		if !initialSet {
			task.initialErr = result
			close(task.initialReady)
		}
	}()
	service, err := deps.auth(ctx)
	if err != nil {
		result = err
		return
	}
	// An interrupted earlier authorization (expired QR code, restarted desktop,
	// lost child) leaves auth.ErrNeedsReconcile. Like the CLI login command,
	// reconcile the retained application configuration instead of failing: either
	// the earlier authorization had completed, or a fresh link is issued for the
	// same application and identity. No new identity is ever created here.
	outcome, err := auth.StartOrReconcile(ctx, service, task.view.ProfileID, recovery)
	if err != nil {
		result = err
		return
	}
	flow := outcome.Flow
	// Always release/reap a retained official child if this workflow is canceled.
	active := ""
	if outcome.Reconciled {
		// Nothing to wait for. Keep a stable tracking key so the page can poll
		// progress exactly as for a real flow; it carries no URL.
		flow = auth.AuthFlow{ID: uuid.NewString(), Kind: automaticReconciledKind}
	} else {
		active = flow.ID
	}
	defer func() {
		if active != "" {
			_ = service.Cancel(context.Background(), active)
		}
	}()
	m.automaticMu.Lock()
	task.initial = flow
	task.view.FlowID = flow.ID
	task.view.Flow = flow
	m.automaticFlows[flow.ID] = task
	initialSet = true
	close(task.initialReady)
	m.automaticMu.Unlock()
	authorized := outcome.Reconciled
	restarted := false
	for !authorized {
		_, err = service.Complete(ctx, flow.ID)
		if err == nil {
			active = ""
			break
		}
		restart := false
		switch {
		case ctx.Err() != nil:
			result = err
			return
		case flow.Kind == "app_config" && errors.Is(err, auth.ErrUserMissing):
			restart = true
		case errors.Is(err, auth.ErrNeedsReconcile):
			// The retained official child ended without a usable result (expired
			// link, lost child). The user may still have finished authorizing, so
			// reconcile instead of failing. Never issue a new link behind a user's
			// back unless the application registration step is what ended.
			_, reconcileErr := service.Reconcile(ctx, task.view.ProfileID)
			switch {
			case reconcileErr == nil:
				active = ""
				authorized = true
				continue
			case errors.Is(reconcileErr, auth.ErrUserMissing) && flow.Kind == "app_config":
				restart = true
			case errors.Is(reconcileErr, auth.ErrUserMissing):
				failureCode = interruptedAuthorizationCode(flow)
				result = err
				return
			default:
				result = fmt.Errorf("%w: %w", auth.ErrReconcileFailed, reconcileErr)
				return
			}
		}
		if !restart || restarted {
			result = err
			return
		}
		restarted = true
		active = ""
		next, startErr := auth.StartOrReconcile(ctx, service, task.view.ProfileID, false)
		if startErr != nil {
			result = startErr
			return
		}
		if next.Reconciled {
			authorized = true
			continue
		}
		flow = next.Flow
		active = flow.ID
		m.updateAutomatic(task, func(v *AutomaticLoginView) { v.Flow = flow })
	}
	m.updateAutomatic(task, func(v *AutomaticLoginView) { v.State = "opening_login" })
	// Match the native login watcher budget: do not keep reporting waiting after
	// its automatic verification window has already expired.
	loginCtx, cancelLogin := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelLogin()
	view, err := deps.open(loginCtx, task.view.ProfileID)
	m.updateAutomatic(task, func(v *AutomaticLoginView) { v.MountID = view.MountID; v.Mountpoint = view.Mountpoint })
	if err != nil {
		failureCode = automaticDiscoveryCode(view)
		result = err
		return
	}
	m.updateAutomatic(task, func(v *AutomaticLoginView) {
		v.State = "waiting_login"
		v.MountID = view.MountID
		v.Mountpoint = view.Mountpoint
	})
	interval := deps.interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		view, err = deps.view(loginCtx, task.view.ProfileID)
		if err != nil {
			result = err
			return
		}
		m.updateAutomatic(task, func(v *AutomaticLoginView) { v.MountID = view.MountID; v.Mountpoint = view.Mountpoint })
		if discoveryLoginReady(loginCtx, view, deps.status) {
			m.updateAutomatic(task, func(v *AutomaticLoginView) { v.State = "ready" })
			return
		}
		switch view.State {
		case "needs_runtime":
			failureCode = "needs_runtime"
			result = ownedlogin.ErrRuntimeUnavailable
			return
		case "unsupported":
			failureCode = "unsupported"
			result = ownedlogin.ErrUnavailable
			return
		case "stale":
			result = model.ErrIdentityChanged
			return
		case "browser_disconnected", "partial":
			failureCode = view.State
			result = ErrUnavailable
			return
		}
		select {
		case <-loginCtx.Done():
			result = loginCtx.Err()
			return
		case <-ticker.C:
		}
	}
}

func automaticDiscoveryCode(view DiscoveryView) string {
	// Only fixed protocol states are returned; persisted or provider strings
	// must not become an error surface containing credentials or response bodies.
	switch view.State {
	case "needs_runtime", "unsupported", "needs_browser", "needs_login", "browser_disconnected", "stale", "partial":
		return view.State
	default:
		return ""
	}
}

// interruptedAuthorizationCode names why a user authorization ended without a
// user: past the link/local deadline it expired, otherwise it is unfinished.
func interruptedAuthorizationCode(flow auth.AuthFlow) string {
	now := time.Now()
	if (!flow.ExpiresAt.IsZero() && now.After(flow.ExpiresAt)) || (!flow.LocalDeadline.IsZero() && now.After(flow.LocalDeadline)) {
		return "expired"
	}
	return "unauthorized"
}

func discoveryLoginReady(ctx context.Context, view DiscoveryView, status func(context.Context, model.MountID) (model.MountStatus, error)) bool {
	if view.State != "complete" || view.Error != "" || !view.Snapshot.Complete || view.MountID == "" {
		return false
	}
	mounted, err := status(ctx, view.MountID)
	return err == nil && mounted.Running
}
func (m *Manager) closeAutomaticLogins() {
	m.automaticMu.Lock()
	m.automaticClosing = true
	for _, task := range m.automaticProfiles {
		task.cancel()
	}
	m.automaticMu.Unlock()
	m.automaticWG.Wait()
}
