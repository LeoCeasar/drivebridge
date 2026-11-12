package manager

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
)

type automaticAuthFixture struct {
	mu          sync.Mutex
	flows       []auth.AuthFlow
	starts      int
	completes   int
	canceled    int
	gate        chan struct{}
	completeErr error
	stageGates  map[string]chan struct{}
	// startErrs[n] is returned by the n-th Start call (nil entries start a flow).
	// reconcileErrs[n] is returned by the n-th Reconcile call; past the end
	// reconcile reports the application as ready without a user.
	startErrs     []error
	appConfigErr  error
	startCalls    int
	reconcileErrs []error
	reconciles    int
}

func (f *automaticAuthFixture) Reconcile(context.Context, model.ProfileID) (auth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.reconciles
	f.reconciles++
	if i < len(f.reconcileErrs) {
		return auth.Identity{}, f.reconcileErrs[i]
	}
	return auth.Identity{}, auth.ErrUserMissing
}
func (f *automaticAuthFixture) Start(ctx context.Context, _ model.ProfileID) (auth.AuthFlow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return auth.AuthFlow{}, err
	}
	call := f.startCalls
	f.startCalls++
	if call < len(f.startErrs) && f.startErrs[call] != nil {
		return auth.AuthFlow{}, f.startErrs[call]
	}
	i := f.starts
	f.starts++
	if i >= len(f.flows) {
		return auth.AuthFlow{}, errors.New("secret error must not escape")
	}
	return f.flows[i], nil
}
func (f *automaticAuthFixture) StartRecovery(ctx context.Context, id model.ProfileID) (auth.AuthFlow, error) {
	return f.Start(ctx, id)
}
func (f *automaticAuthFixture) Complete(ctx context.Context, id string) (auth.Identity, error) {
	gate := f.gate
	if stage := f.stageGates[id]; stage != nil {
		gate = stage
	}
	select {
	case <-ctx.Done():
		return auth.Identity{}, ctx.Err()
	case <-gate:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completes++
	for _, flow := range f.flows {
		if flow.ID == id && flow.Kind == "app_config" {
			if f.appConfigErr != nil {
				return auth.Identity{}, f.appConfigErr
			}
			return auth.Identity{}, auth.ErrUserMissing
		}
	}
	return auth.Identity{}, f.completeErr
}
func (f *automaticAuthFixture) Cancel(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled++
	return nil
}

func automaticFixture(t *testing.T, flows []auth.AuthFlow) (*Manager, *automaticAuthFixture, model.ProfileID, *int) {
	t.Helper()
	m, _, _, _ := fixture(t, nil)
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := &automaticAuthFixture{flows: flows, gate: make(chan struct{})}
	opened := 0
	m.automaticDeps = &automaticLoginDependencies{auth: func(context.Context) (automaticAuth, error) { return a, nil }, open: func(context.Context, model.ProfileID) (DiscoveryView, error) { opened++; return DiscoveryView{}, nil }, view: func(context.Context, model.ProfileID) (DiscoveryView, error) {
		return DiscoveryView{State: "complete", Snapshot: discovery.Snapshot{Complete: true}, MountID: "mount", Mountpoint: "/synthetic/mount"}, nil
	}, status: func(context.Context, model.MountID) (model.MountStatus, error) {
		return model.MountStatus{Running: true}, nil
	}, interval: time.Millisecond, deadline: time.Second}
	return m, a, c.Profiles[0].ID, &opened
}
func awaitAutomatic(t *testing.T, m *Manager, id string, want string) AutomaticLoginView {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		v, err := m.AutomaticLoginStatus(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if v.State == want {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	v, _ := m.AutomaticLoginStatus(context.Background(), id)
	t.Fatalf("state=%s want=%s error=%s", v.State, want, v.Error)
	return v
}
func TestAutomaticLoginSurvivesRequestAndDeduplicates(t *testing.T) {
	m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth", URL: "https://synthetic.invalid"}})
	ctx, cancel := context.WithCancel(context.Background())
	flow, err := m.StartAutomaticLogin(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	duplicate, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil || duplicate.ID != flow.ID {
		t.Fatal(duplicate, err)
	}
	close(a.gate)
	v := awaitAutomatic(t, m, flow.ID, "ready")
	if v.MountID != "mount" || v.Mountpoint != "/synthetic/mount" {
		t.Fatal(v)
	}
	if *opened != 1 {
		t.Fatal("browser opened more than once", *opened)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starts != 1 || a.completes != 1 {
		t.Fatal(a.starts, a.completes)
	}
}
func TestAutomaticLoginAppConfigContinuesUserAuthorization(t *testing.T) {
	m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "application", Kind: "app_config"}, {ID: "user", Kind: "user_auth", URL: "https://synthetic.invalid/user"}})
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(a.gate)
	v := awaitAutomatic(t, m, flow.ID, "ready")
	if v.FlowID != "application" || v.Flow.ID != "user" || *opened != 1 {
		t.Fatal(v, *opened)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.starts != 2 || a.completes != 2 {
		t.Fatal(a.starts, a.completes)
	}
}

func TestAutomaticLoginCurrentPhaseChangesWithoutTrackingKeyChange(t *testing.T) {
	m, a, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "app", Kind: "app_config"}, {ID: "user", Kind: "user_auth", URL: "https://synthetic.invalid/user"}})
	appGate, userGate := make(chan struct{}), make(chan struct{})
	a.stageGates = map[string]chan struct{}{"app": appGate, "user": userGate}
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(appGate)
	deadline := time.Now().Add(time.Second)
	for {
		view, err := m.AutomaticLoginStatus(context.Background(), flow.ID)
		if err != nil {
			t.Fatal(err)
		}
		if view.Flow.ID == "user" {
			if view.FlowID != "app" || view.State != "waiting_authorization" || view.Flow.URL == "" {
				t.Fatal(view)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("user authorization never exposed")
		}
		time.Sleep(time.Millisecond)
	}
	close(userGate)
	awaitAutomatic(t, m, flow.ID, "ready")
}

func TestAutomaticLoginRequiresActuallyReadyMount(t *testing.T) {
	m, a, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth"}})
	m.automaticDeps.deadline = 20 * time.Millisecond
	m.automaticDeps.status = func(context.Context, model.MountID) (model.MountStatus, error) { return model.MountStatus{}, nil }
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(a.gate)
	view := awaitAutomatic(t, m, flow.ID, "failed")
	if view.Error != "expired" {
		t.Fatal(view)
	}
}

func TestAutomaticLoginBrowserErrorUsesOnlyKnownState(t *testing.T) {
	for _, state := range []string{"needs_runtime", "unsupported", "needs_browser", "needs_login", "token=secret"} {
		t.Run(state, func(t *testing.T) {
			m, a, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth"}})
			m.automaticDeps.open = func(context.Context, model.ProfileID) (DiscoveryView, error) {
				return DiscoveryView{State: state}, errors.New("raw token=secret")
			}
			flow, err := m.StartAutomaticLogin(context.Background(), p, false)
			if err != nil {
				t.Fatal(err)
			}
			close(a.gate)
			view := awaitAutomatic(t, m, flow.ID, "failed")
			want := state
			if state == "token=secret" {
				want = "operation_failed"
			}
			if view.Error != want {
				t.Fatal(view)
			}
		})
	}
}
func TestAutomaticLoginMountFailureCannotReportReady(t *testing.T) {
	m, a, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth"}})
	m.automaticDeps.view = func(context.Context, model.ProfileID) (DiscoveryView, error) {
		return DiscoveryView{State: "unsupported", Error: "unsupported", Snapshot: discovery.Snapshot{Complete: true}, MountID: "mount"}, nil
	}
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(a.gate)
	v := awaitAutomatic(t, m, flow.ID, "failed")
	if v.Error != "unsupported" {
		t.Fatal(v)
	}
}
func TestAutomaticLoginErrorRedactedAndRetryAllowed(t *testing.T) {
	m, a, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "first", Kind: "user_auth"}, {ID: "retry", Kind: "user_auth"}})
	a.completeErr = errors.New("token=secret")
	f, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(a.gate)
	v := awaitAutomatic(t, m, f.ID, "failed")
	if v.Error != "operation_failed" {
		t.Fatal(v)
	}
	a.mu.Lock()
	a.completeErr = nil
	a.mu.Unlock()
	f, err = m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil || f.ID != "retry" {
		t.Fatal(f, err)
	}
	awaitAutomatic(t, m, f.ID, "ready")
}
func TestAutomaticLoginCloseCancelsAndWaits(t *testing.T) {
	m, a, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth"}})
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	v, err := m.AutomaticLoginStatus(context.Background(), flow.ID)
	if err != nil || v.State != "failed" || v.Error != "canceled" {
		t.Fatal(v, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.canceled != 1 {
		t.Fatal("child not canceled/reaped", a.canceled)
	}
	if _, err = m.StartAutomaticLogin(context.Background(), p, false); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err = m.AutomaticLoginStatus(context.Background(), "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestAutomaticLoginConcurrentCloseExcludesNewGoroutines(t *testing.T) {
	m, _, p, _ := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth"}})
	var callers sync.WaitGroup
	for i := 0; i < 24; i++ {
		callers.Add(1)
		go func() { defer callers.Done(); _, _ = m.StartAutomaticLogin(context.Background(), p, false) }()
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	callers.Wait()
	if _, err := m.StartAutomaticLogin(context.Background(), p, false); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
