package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

type memoryProfiles struct {
	mu       sync.Mutex
	profiles map[model.ProfileID]model.Profile
}

func (m *memoryProfiles) Profile(_ context.Context, id model.ProfileID) (model.Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.profiles[id]
	if !ok {
		return p, os.ErrNotExist
	}
	return p, nil
}
func (m *memoryProfiles) Bind(_ context.Context, id model.ProfileID, ref, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.profiles[id]
	p.AuthRef = ref
	p.IdentityHash = hash
	m.profiles[id] = p
	return nil
}

type fakeStream struct {
	event  larkcli.AuthEvent
	result larkcli.AuthResult
	err    error
	done   chan struct{}
	once   sync.Once
	reaped bool
	mu     sync.Mutex
}

func (f *fakeStream) Initial() larkcli.AuthEvent { return f.event }
func (f *fakeStream) Wait() (larkcli.AuthResult, error) {
	<-f.done
	f.mu.Lock()
	f.reaped = true
	f.mu.Unlock()
	return f.result, f.err
}
func (f *fakeStream) CancelAndWait() (larkcli.AuthResult, error) { f.finish(); return f.Wait() }
func (f *fakeStream) finish()                                    { f.once.Do(func() { close(f.done) }) }

type fakeProtocol struct {
	config                 larkcli.ConfigMetadata
	identity               larkcli.IdentityMetadata
	verifyErr, errorConfig error
	auth, enroll           *fakeStream
	authCalls, enrollCalls int
	logoutCalls            int
	lastCtx                context.Context
}

func (f *fakeProtocol) InspectConfig(context.Context) (larkcli.ConfigMetadata, error) {
	return f.config, f.errorConfig
}
func (f *fakeProtocol) VerifyIdentity(context.Context) (larkcli.IdentityMetadata, error) {
	return f.identity, f.verifyErr
}
func (f *fakeProtocol) StartEnrollment(ctx context.Context, brand string) (stream, error) {
	f.enrollCalls++
	f.lastCtx = ctx
	watchFake(ctx, f.enroll)
	return f.enroll, nil
}
func (f *fakeProtocol) StartAuth(ctx context.Context, scopes []string) (stream, error) {
	f.authCalls++
	f.lastCtx = ctx
	watchFake(ctx, f.auth)
	return f.auth, nil
}
func fixture(t *testing.T) (*Service, *memoryProfiles, *fakeProtocol, model.ProfileID, string) {
	t.Helper()
	root := privateTemp(t)
	id := model.ProfileID(uuid.NewString())
	p := &memoryProfiles{profiles: map[model.ProfileID]model.Profile{id: {ID: id, Name: "work", Brand: "feishu"}}}
	f := &fakeProtocol{config: larkcli.ConfigMetadata{AppID: "app", Brand: "feishu"}, identity: larkcli.IdentityMetadata{Identity: larkcli.Identity{AppID: "app", Brand: "feishu", UserOpenID: "user"}, Verified: true, Scope: "read write extra"}}
	f.auth = &fakeStream{event: larkcli.AuthEvent{Kind: "user_auth", URL: "https://open.feishu.cn/auth", ExpiresIn: 60}, result: larkcli.AuthResult{UserOpenID: "user", Scope: "read write extra", Granted: []string{"read", "write", "extra"}}, done: make(chan struct{})}
	f.enroll = &fakeStream{event: larkcli.AuthEvent{Kind: "app_config", URL: "https://open.feishu.cn/page/cli?user_code=safe"}, result: larkcli.AuthResult{AppID: "app", Brand: "feishu"}, done: make(chan struct{})}
	s, err := newServiceAt(context.Background(), Options{Executable: "test-cli", ConfigFile: filepath.Join(root, "bridge.json"), Profiles: p, RequiredScopes: []string{"read", "write"}}, root, func(larkcli.EnrollmentOptions) (protocol, error) { return f, nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, p, f, id, root
}
func enroll(t *testing.T, s *Service, f *fakeProtocol, id model.ProfileID) {
	t.Helper()
	flow, err := s.Start(context.Background(), id)
	if err != nil || flow.Kind != "app_config" || !flow.ExpiresAt.IsZero() {
		t.Fatalf("enrollment: %+v %v", flow, err)
	}
	f.enroll.finish()
	_, err = s.Complete(context.Background(), flow.ID)
	if !errors.Is(err, ErrUserMissing) {
		t.Fatalf("application configuration is not user login: %v", err)
	}
}
func login(t *testing.T, s *Service, f *fakeProtocol, id model.ProfileID) Identity {
	t.Helper()
	flow, err := s.Start(context.Background(), id)
	if err != nil || flow.Kind != "user_auth" {
		t.Fatalf("auth start: %+v %v", flow, err)
	}
	f.auth.finish()
	got, err := s.Complete(context.Background(), flow.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func TestAppConfiguredButUserMissing(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	profile, _ := p.Profile(context.Background(), id)
	if profile.AuthRef != "" {
		t.Fatal("registration incorrectly authenticated profile")
	}
	got := login(t, s, f, id)
	if !got.Verified || len(got.Scopes) != 3 {
		t.Fatalf("actual scopes lost: %+v", got)
	}
	binding, err := s.Resolve(context.Background(), id)
	if err != nil || binding.AuthRef == "" {
		t.Fatalf("binding: %+v %v", binding, err)
	}
}
func TestFlowOutlivesInitiatingRequest(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	flow, err := s.Start(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if f.lastCtx.Err() != nil {
		t.Fatal("request cancellation killed retained child")
	}
	f.enroll.finish()
	if _, err = s.Complete(context.Background(), flow.ID); !errors.Is(err, ErrUserMissing) {
		t.Fatal(err)
	}
}
func TestGrantedScopesNotAssumedRequestedOnly(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	f.identity.Scope = "read"
	f.auth.result.Granted = []string{"read", "write"}
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.finish()
	if _, err = s.Complete(context.Background(), flow.ID); !errors.Is(err, ErrMissingScopes) {
		t.Fatalf("missing persisted grant accepted: %v", err)
	}
}
func TestDifferentUserCannotReusePending(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	s.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return nil, ErrReferencesBusy })
	if _, err := s.Start(context.Background(), id); !errors.Is(err, ErrReferencesBusy) {
		t.Fatal(err)
	}
	s.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return func() {}, nil })
	f.auth = &fakeStream{event: larkcli.AuthEvent{Kind: "user_auth", URL: "https://open.feishu.cn/auth", ExpiresIn: 60}, result: larkcli.AuthResult{UserOpenID: "other", Granted: []string{"read", "write"}}, done: make(chan struct{})}
	f.identity.UserOpenID = "other"
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.finish()
	if _, err = s.Complete(context.Background(), flow.ID); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	pr, _ := p.Profile(context.Background(), id)
	if pr.IdentityHash == IdentityHash("feishu", "app", "other") {
		t.Fatal("old queues rebound")
	}
}
func TestEnrollmentSavedButProbeFailsNeedsReconcile(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.enroll.err = errors.New("SECRET DEVICE TOKEN")
	f.enroll.finish()
	if _, err = s.Complete(context.Background(), flow.ID); !errors.Is(err, ErrNeedsReconcile) || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe failure %v", err)
	}
	if _, err = s.Start(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal(err)
	}
	f.verifyErr = larkcli.ErrIdentity
	if _, err = s.Reconcile(context.Background(), id); !errors.Is(err, ErrUserMissing) {
		t.Fatal(err)
	}
	if f.enrollCalls != 1 {
		t.Fatal("blind duplicate registration")
	}
	loginAfterReconcile := f.auth
	loginAfterReconcile.finish()
	f.verifyErr = nil
	login(t, s, f, id)
}
func TestCancelReapsBeforeUnlock(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(context.Background(), flow.ID); err != nil {
		t.Fatal(err)
	}
	f.enroll.mu.Lock()
	reaped := f.enroll.reaped
	f.enroll.mu.Unlock()
	if !reaped {
		t.Fatal("released flow before child reaped")
	}
	if _, err = s.Start(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal(err)
	}
}
func TestRestartReconcilesEnrollmentBeforeRetry(t *testing.T) {
	s, p, f, id, root := fixture(t)
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(context.Background(), flow.ID); err != nil {
		t.Fatal(err)
	}
	s2, err := newServiceAt(context.Background(), s.options, root, func(larkcli.EnrollmentOptions) (protocol, error) { return f, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.profiles = p
	f.verifyErr = larkcli.ErrIdentity
	if _, err = s2.Start(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal(err)
	}
	if _, err = s2.Reconcile(context.Background(), id); !errors.Is(err, ErrUserMissing) {
		t.Fatal(err)
	}
	if f.enrollCalls != 1 {
		t.Fatal("restart created duplicate app")
	}
}
func TestAuthSecretsNeverLogged(t *testing.T) {
	s, _, f, id, root := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if strings.Contains(string(b), "user_code") || strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "access_token") {
			t.Fatalf("secret persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Identity{Brand: "feishu", AppID: "app", UserOpenID: "user", Verified: true})
	if strings.Contains(string(b), "token") {
		t.Fatal(string(b))
	}
}

func privateTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLogoutGuardsAllReferencesAndInvalidatesSharedBinding(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	if err := s.Logout(context.Background(), id); !errors.Is(err, ErrReferencesBusy) {
		t.Fatal(err)
	}
	released := false
	s.guard = GuardFunc(func(_ context.Context, app string, refs []Reference) (func(), error) {
		if app != "app" || len(refs) != 1 {
			t.Fatal("incomplete credential domain")
		}
		return func() { released = true }, nil
	})
	if err := s.Logout(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !released || f.logoutCalls != 1 {
		t.Fatal("missing lease or lifecycle invocation")
	}
	if _, err := s.Status(context.Background(), id); !errors.Is(err, ErrUserMissing) {
		t.Fatal(err)
	}
	if err := s.Logout(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal("unknown/finished logout blindly replayed", err)
	}
}

func (f *fakeProtocol) Logout(context.Context, larkcli.Identity) error { f.logoutCalls++; return nil }

func TestSameIdentitySharesAuthRootAndCoordinator(t *testing.T) {
	s, _, f, id, root := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	binding, _ := s.Resolve(context.Background(), id)
	other := model.ProfileID(uuid.NewString())
	profiles := &memoryProfiles{profiles: map[model.ProfileID]model.Profile{other: {ID: other, Brand: "feishu", Name: "second"}}}
	o := s.options
	o.ConfigFile = filepath.Join(root, "another-config.json")
	o.Profiles = profiles
	s2, err := newServiceAt(context.Background(), o, root, s.factory)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err = s2.Link(context.Background(), other, binding.AuthRef); err != nil {
		t.Fatal(err)
	}
	b2, err := s2.Resolve(context.Background(), other)
	if err != nil || b2.AuthRoot != binding.AuthRoot || b2.IdentityHash != binding.IdentityHash {
		t.Fatalf("split canonical root: %+v %v", b2, err)
	}
	s.guard = GuardFunc(func(_ context.Context, _ string, refs []Reference) (func(), error) {
		if len(refs) != 2 || refs[0].ConfigID == refs[1].ConfigID {
			t.Fatal("cross-config refs lost")
		}
		return nil, ErrReferencesBusy
	})
	if err = s.Logout(context.Background(), id); !errors.Is(err, ErrReferencesBusy) {
		t.Fatal(err)
	}
	s2.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return func() {}, nil })
	if err = s2.Unlink(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if f.logoutCalls != 0 {
		t.Fatal("unlink revoked shared credentials")
	}
	if _, err = s.Resolve(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
func TestExpiredDeviceFlowStartsFresh(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
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
	f.verifyErr = larkcli.ErrIdentity
	if _, err = s.Reconcile(context.Background(), id); !errors.Is(err, ErrUserMissing) {
		t.Fatal(err)
	}
	f.auth = &fakeStream{event: larkcli.AuthEvent{Kind: "user_auth", URL: "https://open.feishu.cn/auth", ExpiresIn: 60}, result: larkcli.AuthResult{UserOpenID: "user", Granted: []string{"read", "write"}}, done: make(chan struct{})}
	next, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == flow.ID || f.authCalls != 2 {
		t.Fatal("expired child/flow reused")
	}
	if err = s.Cancel(context.Background(), next.ID); err != nil {
		t.Fatal(err)
	}
}
func TestAuthorizationCompleteMissingScopesNotSuccess(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	f.auth.result.Granted = nil
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.finish()
	if _, err = s.Complete(context.Background(), flow.ID); !errors.Is(err, ErrMissingScopes) {
		t.Fatal(err)
	}
}
func TestExternalCLIIdentityChangeRequiresRecovery(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	before, _ := p.Profile(context.Background(), id)
	f.identity.UserOpenID = "externally-replaced"
	if _, err := s.Status(context.Background(), id); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	after, _ := p.Profile(context.Background(), id)
	if before.IdentityHash != after.IdentityHash {
		t.Fatal("queue silently rebound")
	}
}
func TestBindRecoveryAfterCanonicalDirectoryMove(t *testing.T) {
	s, p, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.auth.finish()
	active, _ := s.flow(flow.ID)
	<-active.done
	active.mu.Lock()
	r, err := s.readRegistry()
	if err != nil {
		t.Fatal(err)
	}
	i, _ := s.findEnrollment(r, id)
	e := r.Enrollments[i]
	e.CanonicalRef = "auth/identities/" + uuid.NewString() + "/lark"
	e.State = "unknown"
	r.Enrollments[i] = e
	if err = s.saveRegistry(r); err != nil {
		t.Fatal(err)
	}
	source, _ := s.root(e.AuthRef)
	target, _ := s.root(e.CanonicalRef)
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	active.finished = true
	active.release()
	active.cancel()
	active.mu.Unlock()
	if _, err = s.Reconcile(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	profile, _ := p.Profile(context.Background(), id)
	if profile.AuthRef != e.CanonicalRef {
		t.Fatal("canonical crash window changed reference")
	}
	if _, err = s.Resolve(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

func TestServiceCancellationReapsAndReleasesLeaseWithoutComplete(t *testing.T) {
	s, _, f, id, root := fixture(t)
	if _, err := s.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	s.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		release, err := s.lifecycleLease(ctx)
		if err == nil {
			release()
			break
		}
		if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("retained lifecycle lease leaked after service timeout")
		case <-time.After(time.Millisecond):
		}
	}
	f.enroll.mu.Lock()
	reaped := f.enroll.reaped
	f.enroll.mu.Unlock()
	if !reaped {
		t.Fatal("unlocked before reap")
	}
	r, err := s.readRegistry()
	if err != nil || len(r.Enrollments) != 1 || r.Enrollments[0].State != "unknown" {
		t.Fatalf("restart not protected: %+v %v %s", r, err, root)
	}
}

func watchFake(ctx context.Context, f *fakeStream) {
	go func() {
		select {
		case <-f.done:
		case <-ctx.Done():
			f.once.Do(func() { f.err = ctx.Err(); close(f.done) })
		}
	}()
}

func TestUncertainProbeDoesNotEnableFreshAuthorization(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(context.Background(), flow.ID); err != nil {
		t.Fatal(err)
	}
	f.verifyErr = larkcli.ErrProtocol
	if _, err = s.Reconcile(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal(err)
	}
	if _, err = s.Start(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal(err)
	}
	if f.authCalls != 0 || f.enrollCalls != 1 {
		t.Fatal("uncertain probe replayed authorization")
	}
}
func TestTypedNilGuardFailsClosed(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	s.guard = GuardFunc(nil)
	if _, err := s.Start(context.Background(), id); !errors.Is(err, ErrReferencesBusy) {
		t.Fatal(err)
	}
}
func TestRegistryUnknownSchemaAndPathEscapeReadOnly(t *testing.T) {
	s, _, _, _, root := fixture(t)
	path := filepath.Join(root, "auth", "registry.json")
	for _, data := range []string{`{"schema_version":99}`, `{"schema_version":1,"enrollments":[{"id":"11111111-1111-4111-8111-111111111111","config_id":"x","profile_id":"11111111-1111-4111-8111-111111111111","brand":"feishu","auth_ref":"../../outside","state":"unknown"}]}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.readRegistry(); err == nil {
			t.Fatal("malformed/future registry adopted")
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != data {
			t.Fatal("registry was modified")
		}
	}
}

func TestPendingAuthBlocksOldBindingBeforeReconcile(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	s.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return func() {}, nil })
	f.auth = &fakeStream{event: larkcli.AuthEvent{Kind: "user_auth", URL: "https://open.feishu.cn/auth", ExpiresIn: 60}, done: make(chan struct{})}
	flow, err := s.Start(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel(context.Background(), flow.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Status(context.Background(), id); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatal("old binding became usable before explicit recovery", err)
	}
}

func TestExternalAppChangeBlocksReauthBeforeChild(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	s.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return func() {}, nil })
	f.config.AppID = "externally-changed-app"
	if _, err := s.Start(context.Background(), id); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("reauth used stale AppID credential lock", err)
	}
	if f.authCalls != 1 {
		t.Fatal("reauth child started against different AppID")
	}
}
func TestLogoutRechecksIdentityBeforeDestructiveInvocation(t *testing.T) {
	s, _, f, id, _ := fixture(t)
	enroll(t, s, f, id)
	login(t, s, f, id)
	s.guard = GuardFunc(func(context.Context, string, []Reference) (func(), error) { return func() {}, nil })
	f.identity.UserOpenID = "externally-changed-user"
	if err := s.Logout(context.Background(), id); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("logout used stale identity", err)
	}
	if f.logoutCalls != 0 {
		t.Fatal("destructive invocation on changed identity")
	}
}
