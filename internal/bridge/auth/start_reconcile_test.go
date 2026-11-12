package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

func freshUserAuth(f *fakeProtocol, user string) {
	f.auth = &fakeStream{event: larkcli.AuthEvent{Kind: "user_auth", URL: "https://open.feishu.cn/auth", ExpiresIn: 60}, result: larkcli.AuthResult{UserOpenID: user, Granted: []string{"read", "write"}}, done: make(chan struct{})}
}

func allowGuard(s *Service) {
	s.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return func() {}, nil })
}

// interrupt leaves the enrollment as a restart or expired QR code does.
func interrupt(t *testing.T, s *Service, id model.ProfileID) {
	t.Helper()
	flow, err := s.Start(context.Background(), id)
	if err != nil || flow.Kind != "user_auth" {
		t.Fatalf("start: %+v %v", flow, err)
	}
	if err = s.Cancel(context.Background(), flow.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatalf("interrupted authorization must require reconcile: %v", err)
	}
}

func TestStartOrReconcileIssuesFreshLinkAfterExpiredAuthorization(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		s, p, f, id, _ := fixture(t)
		enroll(t, s, f, id)
		if recovery {
			login(t, s, f, id)
			allowGuard(s)
			freshUserAuth(f, "user")
		}
		flow, err := s.Start(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		active, _ := s.flow(flow.ID)
		active.flow.ExpiresAt = time.Now().Add(-time.Second)
		f.auth.finish()
		if _, err = s.Complete(context.Background(), flow.ID); !errors.Is(err, ErrFlowExpired) {
			t.Fatal(err)
		}
		before, _ := p.Profile(context.Background(), id)
		if recovery {
			s.guard = &recoveryGuardFixture{}
		}
		f.verifyErr = larkcli.ErrIdentity
		freshUserAuth(f, "user")
		calls := f.authCalls
		got, err := StartOrReconcile(context.Background(), s, id, recovery)
		if err != nil || got.Reconciled || got.Flow.Kind != "user_auth" || got.Flow.URL == "" {
			t.Fatalf("recovery=%v: %+v %v", recovery, got, err)
		}
		after, _ := p.Profile(context.Background(), id)
		if f.enrollCalls != 1 || f.authCalls != calls+1 || before != after {
			t.Fatal("application or identity was recreated", f.enrollCalls, f.authCalls, calls)
		}
		// The fresh flow completes normally with the same application.
		f.verifyErr = nil
		f.auth.finish()
		if identity, err := s.Complete(context.Background(), got.Flow.ID); err != nil || identity.UserOpenID != "user" {
			t.Fatal(identity, err)
		}
	}
}

func TestStartOrReconcileDetectsAuthorizationCompletedBeforeInterruption(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	interrupt(t, s, id)
	calls := f.authCalls
	// The user actually finished in the browser: the official CLI now verifies.
	f.verifyErr = nil
	got, err := StartOrReconcile(context.Background(), s, id, false)
	if err != nil || !got.Reconciled || got.Identity.UserOpenID != "user" || got.Flow.ID != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if f.authCalls != calls || f.enrollCalls != 1 {
		t.Fatal("completed authorization asked the user again", f.authCalls, calls, f.enrollCalls)
	}
	bound, _ := p.Profile(context.Background(), id)
	if bound.AuthRef == "" || bound.IdentityHash != IdentityHash("feishu", "app", "user") {
		t.Fatalf("identity not bound: %+v", bound)
	}
	if _, err = s.Status(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestStartOrReconcileFailureKeepsStateAndIsClassified(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	interrupt(t, s, id)
	calls := f.authCalls
	f.verifyErr = larkcli.ErrProtocol
	_, err := StartOrReconcile(context.Background(), s, id, false)
	if !errors.Is(err, ErrReconcileFailed) || !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal(err)
	}
	if bound, _ := p.Profile(context.Background(), id); bound.AuthRef != "" {
		t.Fatal("uncertain reconcile bound an identity")
	}
	if f.authCalls != calls || f.enrollCalls != 1 {
		t.Fatal("uncertain reconcile started a new authorization")
	}
	// Once the CLI recovers, the same call path succeeds without losing the app.
	f.verifyErr = larkcli.ErrIdentity
	freshUserAuth(f, "user")
	if got, err := StartOrReconcile(context.Background(), s, id, false); err != nil || got.Flow.Kind != "user_auth" {
		t.Fatal(got, err)
	}
}

func TestStartOrReconcileRefusesDifferentIdentity(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	allowGuard(s)
	bound, _ := p.Profile(context.Background(), id)
	freshUserAuth(f, "user")
	interrupt(t, s, id)
	f.identity.UserOpenID = "other"
	_, err := StartOrReconcile(context.Background(), s, id, false)
	if !errors.Is(err, ErrReconcileFailed) || !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	if after, _ := p.Profile(context.Background(), id); after != bound {
		t.Fatal("identity silently rebound", after, bound)
	}
}

func TestReconcileMissingScopesAllowsReauthorizationOfSameIdentity(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	interrupt(t, s, id)
	f.identity.Scope = "read"
	if _, err := s.Reconcile(context.Background(), id); !errors.Is(err, ErrUserMissing) {
		t.Fatalf("under-scoped authorization left unrecoverable: %v", err)
	}
	if bound, _ := p.Profile(context.Background(), id); bound.AuthRef != "" {
		t.Fatal("under-scoped identity was bound")
	}
	f.identity.Scope = "read write extra"
	freshUserAuth(f, "user")
	if got, err := StartOrReconcile(context.Background(), s, id, false); err != nil || got.Flow.Kind != "user_auth" {
		t.Fatal(got, err)
	}
}

type loopStarter struct{ starts, reconciles int }

func (l *loopStarter) Start(context.Context, model.ProfileID) (AuthFlow, error) {
	l.starts++
	return AuthFlow{}, ErrNeedsReconcile
}
func (l *loopStarter) StartRecovery(ctx context.Context, id model.ProfileID) (AuthFlow, error) {
	return l.Start(ctx, id)
}
func (l *loopStarter) Reconcile(context.Context, model.ProfileID) (Identity, error) {
	l.reconciles++
	return Identity{}, ErrUserMissing
}

func TestStartOrReconcileIsBounded(t *testing.T) {
	l := &loopStarter{}
	if _, err := StartOrReconcile(context.Background(), l, "profile", false); !errors.Is(err, ErrReconcileFailed) {
		t.Fatal(err)
	}
	if l.starts != maxReconcileAttempts || l.reconciles != maxReconcileAttempts {
		t.Fatal(l.starts, l.reconciles)
	}
}
