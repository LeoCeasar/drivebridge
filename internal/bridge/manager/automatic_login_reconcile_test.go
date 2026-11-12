package manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/model"
)

// Regression: after a QR code expired and the desktop restarted, the retained
// enrollment is "needs reconcile". The management page used to surface that as a
// bare failure; it must reconcile like the CLI login command.

func TestAutomaticLoginInterruptedFlowIssuesFreshLink(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "fresh", Kind: "user_auth", URL: "https://synthetic.invalid/fresh"}})
		a.startErrs = []error{auth.ErrNeedsReconcile}
		a.reconcileErrs = []error{auth.ErrUserMissing}
		flow, err := m.StartAutomaticLogin(context.Background(), p, recovery)
		if err != nil {
			t.Fatalf("recovery=%v: %v", recovery, err)
		}
		if flow.ID != "fresh" || flow.URL == "" || flow.Kind != "user_auth" {
			t.Fatalf("recovery=%v: want fresh authorization link, got %+v", recovery, flow)
		}
		close(a.gate)
		awaitAutomatic(t, m, flow.ID, "ready")
		a.mu.Lock()
		if a.reconciles != 1 || a.starts != 1 || a.completes != 1 || *opened != 1 {
			t.Fatal(a.reconciles, a.starts, a.completes, *opened)
		}
		a.mu.Unlock()
	}
}

func TestAutomaticLoginCompletedAuthorizationSkipsNewLink(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		m, a, p, opened := automaticFixture(t, nil)
		a.startErrs = []error{auth.ErrNeedsReconcile}
		a.reconcileErrs = []error{nil}
		flow, err := m.StartAutomaticLogin(context.Background(), p, recovery)
		if err != nil {
			t.Fatalf("recovery=%v: %v", recovery, err)
		}
		if flow.Kind != "reconciled" || flow.URL != "" {
			t.Fatalf("recovery=%v: no new authorization may be requested: %+v", recovery, flow)
		}
		if _, perr := uuid.Parse(flow.ID); perr != nil {
			t.Fatal("tracking key must be a UUID the page can poll:", flow.ID)
		}
		v := awaitAutomatic(t, m, flow.ID, "ready")
		if v.FlowID != flow.ID || v.MountID != "mount" || v.Error != "" {
			t.Fatal(v)
		}
		a.mu.Lock()
		// Verification, owned login and mount continue; no second authorization.
		if a.reconciles != 1 || a.starts != 0 || a.completes != 0 || a.canceled != 0 || *opened != 1 {
			t.Fatal(a.reconciles, a.starts, a.completes, a.canceled, *opened)
		}
		a.mu.Unlock()
	}
}

func TestAutomaticLoginReconcileFailureIsSpecificAndRetryable(t *testing.T) {
	m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "retry", Kind: "user_auth", URL: "https://synthetic.invalid/retry"}})
	a.startErrs = []error{auth.ErrNeedsReconcile, auth.ErrNeedsReconcile}
	a.reconcileErrs = []error{errors.Join(auth.ErrNeedsReconcile, errors.New("token=secret"))}
	_, err := m.StartAutomaticLogin(context.Background(), p, false)
	if !errors.Is(err, auth.ErrReconcileFailed) || !errors.Is(err, auth.ErrNeedsReconcile) {
		t.Fatal("reconcile failure lost its identity:", err)
	}
	if code := automaticError(err); code != "auth_reconcile_failed" {
		t.Fatal("code:", code)
	}
	a.mu.Lock()
	if a.starts != 0 || *opened != 0 {
		t.Fatal("must not issue a link or open login after a failed reconcile", a.starts, *opened)
	}
	a.mu.Unlock()
	// The failed workflow is finished: the next click retries and now succeeds.
	a.reconcileErrs = append(a.reconcileErrs, auth.ErrUserMissing)
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil || flow.ID != "retry" {
		t.Fatal(flow, err)
	}
	close(a.gate)
	awaitAutomatic(t, m, flow.ID, "ready")
}

func TestAutomaticLoginReconcileKeepsBusyAndIdentityMeaning(t *testing.T) {
	for cause, want := range map[error]string{auth.ErrBusy: "pending", model.ErrIdentityChanged: "identity_changed"} {
		err := errors.Join(auth.ErrReconcileFailed, cause)
		if got := automaticError(err); got != want {
			t.Fatalf("%v: got %s want %s", cause, got, want)
		}
	}
}

func TestAutomaticLoginReconcileDoesNotLoopForever(t *testing.T) {
	m, a, p, _ := automaticFixture(t, nil)
	a.startErrs = []error{auth.ErrNeedsReconcile, auth.ErrNeedsReconcile, auth.ErrNeedsReconcile, auth.ErrNeedsReconcile}
	a.reconcileErrs = []error{auth.ErrUserMissing, auth.ErrUserMissing, auth.ErrUserMissing, auth.ErrUserMissing}
	_, err := m.StartAutomaticLogin(context.Background(), p, false)
	if !errors.Is(err, auth.ErrReconcileFailed) {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.startCalls != 3 || a.reconciles != 3 {
		t.Fatal(a.startCalls, a.reconciles)
	}
}

func TestAutomaticLoginMidFlowLostChildThatActuallyCompleted(t *testing.T) {
	m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth", URL: "https://synthetic.invalid/auth"}})
	a.completeErr = auth.ErrNeedsReconcile
	a.reconcileErrs = []error{nil}
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(a.gate)
	awaitAutomatic(t, m, flow.ID, "ready")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reconciles != 1 || a.starts != 1 || *opened != 1 {
		t.Fatal(a.reconciles, a.starts, *opened)
	}
}

func TestAutomaticLoginMidFlowExpiryDoesNotSilentlyIssueNewLink(t *testing.T) {
	for _, tc := range []struct {
		name string
		flow auth.AuthFlow
		want string
	}{
		{"expired", auth.AuthFlow{ID: "official", Kind: "user_auth", URL: "https://synthetic.invalid/auth", ExpiresAt: time.Now().Add(-time.Minute)}, "expired"},
		{"unfinished", auth.AuthFlow{ID: "official", Kind: "user_auth", URL: "https://synthetic.invalid/auth"}, "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, a, p, opened := automaticFixture(t, []auth.AuthFlow{tc.flow, {ID: "unwanted", Kind: "user_auth", URL: "https://synthetic.invalid/other"}})
			a.completeErr = auth.ErrNeedsReconcile
			a.reconcileErrs = []error{auth.ErrUserMissing}
			flow, err := m.StartAutomaticLogin(context.Background(), p, false)
			if err != nil {
				t.Fatal(err)
			}
			close(a.gate)
			v := awaitAutomatic(t, m, flow.ID, "failed")
			if v.Error != tc.want {
				t.Fatal(v)
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.starts != 1 || a.reconciles != 1 || *opened != 0 {
				t.Fatal(a.starts, a.reconciles, *opened)
			}
		})
	}
}

func TestAutomaticLoginMidFlowReconcileFailureIsSpecific(t *testing.T) {
	m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "official", Kind: "user_auth", URL: "https://synthetic.invalid/auth"}})
	a.completeErr = auth.ErrNeedsReconcile
	a.reconcileErrs = []error{auth.ErrNeedsReconcile}
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	close(a.gate)
	v := awaitAutomatic(t, m, flow.ID, "failed")
	if v.Error != "auth_reconcile_failed" || *opened != 0 {
		t.Fatal(v, *opened)
	}
}

func TestAutomaticLoginAppRegistrationInterruptedContinuesToUserLink(t *testing.T) {
	m, a, p, opened := automaticFixture(t, []auth.AuthFlow{{ID: "app", Kind: "app_config"}, {ID: "user", Kind: "user_auth", URL: "https://synthetic.invalid/user"}})
	// The registration child ends without a result, but the application was
	// saved: reconcile says "app ready, user missing" and the workflow continues
	// with a user link without registering another application.
	a.appConfigErr = auth.ErrNeedsReconcile
	a.reconcileErrs = []error{auth.ErrUserMissing, auth.ErrUserMissing}
	close(a.gate)
	flow, err := m.StartAutomaticLogin(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	v := awaitAutomatic(t, m, flow.ID, "ready")
	if v.FlowID != "app" || v.Flow.ID != "user" || *opened != 1 {
		t.Fatal(v, *opened)
	}
}
