package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/lib/larkcli"
)

type Options struct {
	Executable, ConfigFile string
	Profiles               ProfileStore
	RequiredScopes         []string
	Guard                  LifecycleGuard
}
type AuthFlow struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	URL           string    `json:"url"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	LocalDeadline time.Time `json:"local_deadline"`
}
type stream interface {
	Initial() larkcli.AuthEvent
	Wait() (larkcli.AuthResult, error)
	CancelAndWait() (larkcli.AuthResult, error)
}
type protocol interface {
	InspectConfig(context.Context) (larkcli.ConfigMetadata, error)
	VerifyIdentity(context.Context) (larkcli.IdentityMetadata, error)
	StartEnrollment(context.Context, string) (stream, error)
	StartAuth(context.Context, []string) (stream, error)
	Logout(context.Context, larkcli.Identity) error
}
type protocolFactory func(larkcli.EnrollmentOptions) (protocol, error)
type cliProtocol struct {
	r       *larkcli.Runner
	options larkcli.EnrollmentOptions
}
type cliStream struct{ s *larkcli.AuthStream }

func (c cliStream) Initial() larkcli.AuthEvent                 { return c.s.Initial }
func (c cliStream) Wait() (larkcli.AuthResult, error)          { return c.s.Wait() }
func (c cliStream) CancelAndWait() (larkcli.AuthResult, error) { return c.s.CancelAndWait() }
func (c cliProtocol) InspectConfig(ctx context.Context) (larkcli.ConfigMetadata, error) {
	return c.r.InspectConfig(ctx)
}
func (c cliProtocol) VerifyIdentity(ctx context.Context) (larkcli.IdentityMetadata, error) {
	return c.r.VerifyIdentity(ctx)
}
func (c cliProtocol) StartEnrollment(ctx context.Context, b string) (stream, error) {
	s, err := c.r.StartEnrollment(ctx, b)
	if err != nil {
		return nil, err
	}
	return cliStream{s}, nil
}
func (c cliProtocol) StartAuth(ctx context.Context, scopes []string) (stream, error) {
	s, err := c.r.StartAuth(ctx, scopes)
	if err != nil {
		return nil, err
	}
	return cliStream{s}, nil
}

type retainedFlow struct {
	mu         sync.Mutex
	flow       AuthFlow
	enrollment enrollment
	child      stream
	cancel     context.CancelFunc
	release    func()
	done       chan struct{}
	finalized  chan struct{}
	result     larkcli.AuthResult
	err        error
	finished   bool
	identity   Identity
	finalErr   error
}
type Service struct {
	options          Options
	domain, configID string
	profiles         ProfileStore
	guard            LifecycleGuard
	coordinator      *Coordinator
	factory          protocolFactory
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	flows            map[string]*retainedFlow
	closed           bool
}

func New(ctx context.Context, o Options) (*Service, error) {
	domain, err := DefaultDomain()
	if err != nil {
		return nil, err
	}
	return newServiceAt(ctx, o, domain, func(o larkcli.EnrollmentOptions) (protocol, error) {
		r, err := larkcli.NewEnrollment(o)
		if err != nil {
			return nil, err
		}
		return cliProtocol{r: r, options: o}, nil
	})
}
func newServiceAt(ctx context.Context, o Options, domain string, f protocolFactory) (*Service, error) {
	if !filepath.IsAbs(domain) || !filepath.IsAbs(o.ConfigFile) || o.Executable == "" || f == nil || len(o.RequiredScopes) == 0 {
		return nil, ErrInvalid
	}
	for _, v := range o.RequiredScopes {
		if v == "" || strings.ContainsAny(v, " \t\n\r") {
			return nil, ErrInvalid
		}
	}
	o.RequiredScopes = append([]string(nil), o.RequiredScopes...)
	sort.Strings(o.RequiredScopes)
	o.RequiredScopes = compact(o.RequiredScopes)
	if o.Profiles == nil {
		o.Profiles = FileProfiles{o.ConfigFile}
	} else if nilValue(o.Profiles) {
		return nil, ErrInvalid
	}
	c, err := newCoordinatorAt(domain)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Service{options: o, domain: domain, configID: key(filepath.Clean(o.ConfigFile)), profiles: o.Profiles, guard: o.Guard, coordinator: c, factory: f, ctx: ctx, cancel: cancel, flows: map[string]*retainedFlow{}}
	return s, nil
}
func (s *Service) Coordinator() *Coordinator { return s.coordinator }

// lifecycleLease serializes reference changes and retained authorizations across
// all product configs. It is separate from the credential locks used by children.
func (s *Service) lifecycleLease(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l, err := privateLock(filepath.Join(s.coordinator.root, "lifecycle.lock"))
	if err != nil {
		return nil, err
	}
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrBusy
	}
	var once sync.Once
	return func() { once.Do(func() { _ = l.Unlock() }) }, nil
}
func (s *Service) runner(e enrollment) (protocol, error) {
	root, err := s.root(e.AuthRef)
	if err != nil {
		return nil, err
	}
	return s.factory(larkcli.EnrollmentOptions{Executable: s.options.Executable, AuthRoot: root, AuthRef: e.AuthRef, AppID: e.AppID, Coordinator: s.coordinator})
}
func (s *Service) Start(ctx context.Context, id model.ProfileID) (AuthFlow, error) {
	return s.start(ctx, id, false)
}

// StartRecovery is an explicit preservation workflow for an already bound
// identity. It cannot create an app, change identity, or discard pending work.
func (s *Service) StartRecovery(ctx context.Context, id model.ProfileID) (AuthFlow, error) {
	return s.start(ctx, id, true)
}
func (s *Service) start(ctx context.Context, id model.ProfileID, recovery bool) (AuthFlow, error) {
	if err := ctx.Err(); err != nil {
		return AuthFlow{}, err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || s.ctx.Err() != nil {
		return AuthFlow{}, context.Canceled
	}
	p, err := s.profiles.Profile(ctx, id)
	if err != nil {
		return AuthFlow{}, err
	}
	if recovery && (p.AuthRef == "" || p.IdentityHash == "") {
		return AuthFlow{}, model.ErrIdentityChanged
	}
	if !validID(string(id)) || (p.Brand != "feishu" && p.Brand != "lark") {
		return AuthFlow{}, ErrInvalid
	}
	release, err := s.lifecycleLease(ctx)
	if err != nil {
		return AuthFlow{}, err
	}
	owned := true
	defer func() {
		if owned {
			release()
		}
	}()
	r, err := s.readRegistry()
	if err != nil {
		return AuthFlow{}, err
	}
	i, exists := s.findEnrollment(r, id)
	var e enrollment
	if exists {
		e = r.Enrollments[i]
		if e.State != "app_ready" {
			return AuthFlow{}, ErrNeedsReconcile
		}
		if e.ExpectedHash != p.IdentityHash || e.Brand != p.Brand {
			return AuthFlow{}, model.ErrIdentityChanged
		}
	} else {
		e = enrollment{ID: uuid.NewString(), ConfigID: s.configID, ProfileID: id, Brand: p.Brand, ExpectedHash: p.IdentityHash, State: "prepared"}
		if p.AuthRef != "" {
			binding, err := s.resolve(ctx, id, true, true)
			if err != nil {
				return AuthFlow{}, err
			}
			e.AuthRef = binding.AuthRef
			e.AppID = binding.AppID
		} else {
			e.AuthRef = "auth/enrollments/" + e.ID + "/lark"
		}
		r.Enrollments = append(r.Enrollments, e)
		i = len(r.Enrollments) - 1
	}
	e.Recovery = recovery
	guardRelease, err := s.guardFlow(ctx, r, e.AppID, recovery)
	if err != nil {
		return AuthFlow{}, err
	}
	combined := release
	release = func() { guardRelease(); combined() }
	root, err := s.root(e.AuthRef)
	if err != nil {
		return AuthFlow{}, err
	}
	if err = platform.EnsurePrivateDir(root); err != nil {
		return AuthFlow{}, err
	}
	e.State = "prepared"
	r.Enrollments[i] = e
	if err = s.saveRegistry(r); err != nil {
		return AuthFlow{}, err
	}
	driver, err := s.runner(e)
	if err != nil {
		return AuthFlow{}, err
	}
	if e.AppID != "" {
		meta, probeErr := driver.InspectConfig(ctx)
		if probeErr != nil || meta.AppID != e.AppID || meta.Brand != e.Brand {
			e.State = "unknown"
			r.Enrollments[i] = e
			if err = s.saveRegistry(r); err != nil {
				return AuthFlow{}, errors.Join(model.ErrIdentityChanged, err)
			}
			return AuthFlow{}, model.ErrIdentityChanged
		}
	}
	// The initiating HTTP/CLI request may return once it has the URL. Only the
	// service lifetime and this separately disclosed local deadline own the child.
	childCtx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	var child stream
	if e.AppID == "" {
		child, err = driver.StartEnrollment(childCtx, e.Brand)
	} else {
		child, err = driver.StartAuth(childCtx, s.options.RequiredScopes)
	}
	if err != nil {
		cancel()
		e.State = "unknown"
		r.Enrollments[i] = e
		if saveErr := s.saveRegistry(r); saveErr != nil {
			return AuthFlow{}, errors.Join(ErrNeedsReconcile, saveErr)
		}
		return AuthFlow{}, ErrNeedsReconcile
	}
	event := child.Initial()
	now := time.Now()
	deadline, _ := childCtx.Deadline()
	flow := AuthFlow{ID: uuid.NewString(), Kind: event.Kind, URL: event.URL, LocalDeadline: deadline}
	if event.Kind == "user_auth" {
		flow.ExpiresAt = now.Add(time.Duration(event.ExpiresIn) * time.Second)
	}
	e.State = "active"
	r.Enrollments[i] = e
	if err = s.saveRegistry(r); err != nil {
		_, _ = child.CancelAndWait()
		cancel()
		return AuthFlow{}, errors.Join(ErrNeedsReconcile, err)
	}
	f := &retainedFlow{flow: flow, enrollment: e, child: child, cancel: cancel, release: release, done: make(chan struct{}), finalized: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_, _ = child.CancelAndWait()
		cancel()
		return AuthFlow{}, context.Canceled
	}
	s.flows[flow.ID] = f
	s.mu.Unlock()
	go func() {
		f.result, f.err = child.Wait()
		close(f.done)
		select {
		case <-f.finalized:
		case <-childCtx.Done():
			_ = s.Cancel(context.Background(), flow.ID)
		}
	}()
	owned = false
	return flow, nil
}
func (s *Service) flow(id string) (*retainedFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.flows[id]
	if f == nil {
		return nil, ErrFlowNotFound
	}
	return f, nil
}
func (s *Service) Complete(ctx context.Context, id string) (Identity, error) {
	f, err := s.flow(id)
	if err != nil {
		return Identity{}, err
	}
	select {
	case <-ctx.Done():
		return Identity{}, ctx.Err()
	case <-f.done:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finished {
		return f.identity, f.finalErr
	}
	defer f.cancel()
	defer f.release()
	f.finished = true
	defer close(f.finalized)
	r, err := s.readRegistry()
	if err != nil {
		f.finalErr = err
		return Identity{}, err
	}
	i, ok := s.findEnrollment(r, f.enrollment.ProfileID)
	if !ok {
		f.finalErr = ErrNeedsReconcile
		return Identity{}, f.finalErr
	}
	e := r.Enrollments[i]
	fail := func(cause error) (Identity, error) {
		if errors.Is(cause, model.ErrIdentityChanged) {
			found := false
			for _, ref := range r.Disabled {
				if ref == e.AuthRef {
					found = true
				}
			}
			if !found && validRef(e.AuthRef, false) {
				r.Disabled = append(r.Disabled, e.AuthRef)
			}
		}
		e.State = "unknown"
		r.Enrollments[i] = e
		err := s.saveRegistry(r)
		f.finalErr = errors.Join(cause, err)
		return Identity{}, f.finalErr
	}
	if f.err != nil {
		return fail(ErrNeedsReconcile)
	}
	driver, err := s.runner(e)
	if err != nil {
		return fail(ErrNeedsReconcile)
	}
	meta, err := driver.InspectConfig(ctx)
	if err != nil || meta.Brand != e.Brand || meta.AppID == "" || (e.AppID != "" && e.AppID != meta.AppID) {
		return fail(ErrNeedsReconcile)
	}
	e.AppID = meta.AppID
	r.Enrollments[i] = e
	if f.flow.Kind == "app_config" {
		if f.result.AppID != meta.AppID || f.result.Brand != meta.Brand {
			return fail(ErrNeedsReconcile)
		}
		e.State = "app_ready"
		r.Enrollments[i] = e
		if err = s.saveRegistry(r); err != nil {
			f.finalErr = err
			return Identity{}, err
		}
		f.finalErr = ErrUserMissing
		return Identity{}, ErrUserMissing
	}
	if !f.flow.ExpiresAt.IsZero() && time.Now().After(f.flow.ExpiresAt) {
		return fail(ErrFlowExpired)
	}
	if len(f.result.Missing) > 0 || !hasScopes(f.result.Granted, s.options.RequiredScopes) {
		return fail(ErrMissingScopes)
	}
	driver, err = s.runner(e)
	if err != nil {
		return fail(ErrNeedsReconcile)
	}
	verified, err := driver.VerifyIdentity(ctx)
	if err != nil {
		return fail(ErrNeedsReconcile)
	}
	identity, err := s.validateIdentity(e, verified)
	if err != nil {
		return fail(err)
	}
	if f.result.UserOpenID != identity.UserOpenID {
		return fail(model.ErrIdentityChanged)
	}
	identity, err = s.bind(ctx, &r, i, identity)
	if err != nil {
		return fail(err)
	}
	f.identity = identity
	return identity, nil
}
func (s *Service) validateIdentity(e enrollment, v larkcli.IdentityMetadata) (Identity, error) {
	if !v.Verified || v.AppID != e.AppID || v.Brand != e.Brand || v.UserOpenID == "" {
		return Identity{}, model.ErrIdentityChanged
	}
	if e.ExpectedHash != "" && e.ExpectedHash != IdentityHash(v.Brand, v.AppID, v.UserOpenID) {
		return Identity{}, model.ErrIdentityChanged
	}
	granted := scopes(v.Scope)
	if !hasScopes(granted, s.options.RequiredScopes) {
		return Identity{}, ErrMissingScopes
	}
	return Identity{Brand: v.Brand, AppID: v.AppID, UserOpenID: v.UserOpenID, Verified: true, Scopes: granted}, nil
}
func (s *Service) bind(ctx context.Context, r *registry, index int, identity Identity) (Identity, error) {
	e := r.Enrollments[index]
	hash := IdentityHash(identity.Brand, identity.AppID, identity.UserOpenID)
	var record model.IdentityRecord
	exists := false
	for _, v := range r.Identities {
		if v.AppID == identity.AppID && v.UserOpenID == identity.UserOpenID {
			if v.Brand != identity.Brand || v.IdentityHash != hash {
				return Identity{}, model.ErrIdentityChanged
			}
			record = v
			exists = true
			break
		}
	}
	if !exists {
		id := uuid.NewString()
		if e.CanonicalRef != "" {
			id = strings.Split(e.CanonicalRef, "/")[2]
		}
		record = model.IdentityRecord{ID: id, AuthRef: "auth/identities/" + id + "/lark", VerifiedIdentity: model.VerifiedIdentity{Brand: identity.Brand, AppID: identity.AppID, UserOpenID: identity.UserOpenID, IdentityHash: hash}}
	}
	source, err := s.root(e.AuthRef)
	if err != nil {
		return Identity{}, err
	}
	target, err := s.root(record.AuthRef)
	if err != nil {
		return Identity{}, err
	}
	if e.AuthRef != record.AuthRef {
		if exists {
			canonical := e
			canonical.AuthRef = record.AuthRef
			driver, err := s.runner(canonical)
			if err != nil {
				return Identity{}, err
			}
			v, err := driver.VerifyIdentity(ctx)
			if err != nil {
				return Identity{}, ErrNeedsReconcile
			}
			if _, err = s.validateIdentity(enrollment{AppID: identity.AppID, Brand: identity.Brand, ExpectedHash: hash}, v); err != nil {
				return Identity{}, err
			}
		} else {
			e.CanonicalRef = record.AuthRef
			r.Enrollments[index] = e
			if err = s.saveRegistry(*r); err != nil {
				return Identity{}, err
			}
			if err = platform.EnsurePrivateDir(filepath.Dir(target)); err != nil {
				return Identity{}, err
			}
			if _, err = os.Lstat(target); os.IsNotExist(err) {
				if err = os.Rename(source, target); err != nil {
					return Identity{}, err
				}
				if err = platform.SyncDir(filepath.Dir(source)); err != nil {
					return Identity{}, err
				}
				if err = platform.SyncDir(filepath.Dir(target)); err != nil {
					return Identity{}, err
				}
			} else if err != nil {
				return Identity{}, err
			} else {
				return Identity{}, ErrNeedsReconcile
			}
		}
	}
	if !exists {
		r.Identities = append(r.Identities, record)
	}
	current, err := s.profiles.Profile(ctx, e.ProfileID)
	if err != nil {
		return Identity{}, err
	}
	if current.Brand != identity.Brand || (current.AuthRef != "" && (current.AuthRef != record.AuthRef || current.IdentityHash != hash)) {
		return Identity{}, model.ErrIdentityChanged
	}
	ref := Reference{ConfigID: s.configID, ConfigFile: s.options.ConfigFile, ProfileID: e.ProfileID, AuthRef: record.AuthRef, IdentityHash: hash}
	found := false
	for i, v := range r.References {
		if v.ConfigID == ref.ConfigID && v.ProfileID == ref.ProfileID {
			if v.AuthRef != ref.AuthRef || v.IdentityHash != ref.IdentityHash {
				return Identity{}, model.ErrIdentityChanged
			}
			r.References[i] = ref
			found = true
		}
	}
	if !found {
		r.References = append(r.References, ref)
	}
	if err = s.saveRegistry(*r); err != nil {
		return Identity{}, err
	}
	if err = s.profiles.Bind(ctx, e.ProfileID, record.AuthRef, hash); err != nil {
		return Identity{}, err
	}
	disabled := r.Disabled[:0]
	for _, ref := range r.Disabled {
		if ref != record.AuthRef {
			disabled = append(disabled, ref)
		}
	}
	r.Disabled = disabled
	r.Enrollments = append(r.Enrollments[:index], r.Enrollments[index+1:]...)
	if err = s.saveRegistry(*r); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

// Reconcile inspects retained on-disk configuration after cancellation, failure,
// or restart. It never registers/logs in again or copies CLI-owned credentials.
func (s *Service) Reconcile(ctx context.Context, id model.ProfileID) (Identity, error) {
	release, err := s.lifecycleLease(ctx)
	if err != nil {
		return Identity{}, err
	}
	defer release()
	r, err := s.readRegistry()
	if err != nil {
		return Identity{}, err
	}
	i, ok := s.findEnrollment(r, id)
	if !ok {
		return s.Status(ctx, id)
	}
	e := r.Enrollments[i]
	if e.CanonicalRef != "" {
		canonical, pathErr := s.root(e.CanonicalRef)
		if pathErr != nil {
			return Identity{}, pathErr
		}
		if _, err = os.Lstat(canonical); err == nil {
			e.AuthRef = e.CanonicalRef
			r.Enrollments[i] = e
		}
	}
	driver, err := s.runner(e)
	if err != nil {
		return Identity{}, err
	}
	meta, err := driver.InspectConfig(ctx)
	if err != nil || meta.Brand != e.Brand || (e.AppID != "" && meta.AppID != e.AppID) {
		return Identity{}, ErrNeedsReconcile
	}
	e.AppID = meta.AppID
	r.Enrollments[i] = e
	driver, err = s.runner(e)
	if err != nil {
		return Identity{}, err
	}
	v, err := driver.VerifyIdentity(ctx)
	if err != nil {
		if !errors.Is(err, larkcli.ErrIdentity) {
			return Identity{}, ErrNeedsReconcile
		}
		e.State = "app_ready"
		r.Enrollments[i] = e
		if err = s.saveRegistry(r); err != nil {
			return Identity{}, err
		}
		return Identity{}, ErrUserMissing
	}
	identity, err := s.validateIdentity(e, v)
	if errors.Is(err, ErrMissingScopes) {
		// The verified user matches the expected identity (validateIdentity checks
		// the identity hash before scopes) but did not grant every required scope.
		// Keep the application and let the same identity authorize again, instead
		// of leaving the enrollment unrecoverable.
		e.State = "app_ready"
		r.Enrollments[i] = e
		if err = s.saveRegistry(r); err != nil {
			return Identity{}, err
		}
		return Identity{}, ErrUserMissing
	}
	if err != nil {
		return Identity{}, err
	}
	return s.bind(ctx, &r, i, identity)
}
func (s *Service) Status(ctx context.Context, id model.ProfileID) (Identity, error) {
	b, err := s.Resolve(ctx, id)
	if err != nil {
		return Identity{}, err
	}
	driver, err := s.factory(larkcli.EnrollmentOptions{Executable: s.options.Executable, AuthRef: b.AuthRef, AuthRoot: b.AuthRoot, AppID: b.AppID, Coordinator: s.coordinator})
	if err != nil {
		return Identity{}, err
	}
	v, err := driver.VerifyIdentity(ctx)
	if err != nil {
		if errors.Is(err, larkcli.ErrIdentity) {
			return Identity{}, model.ErrIdentityChanged
		}
		return Identity{}, err
	}
	return s.validateIdentity(enrollment{Brand: b.Brand, AppID: b.AppID, ExpectedHash: b.IdentityHash}, v)
}
func (s *Service) ManagedRunner(ctx context.Context, id model.ProfileID) (*larkcli.Runner, error) {
	identity, err := s.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	binding, err := s.Resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return larkcli.NewManaged(larkcli.ManagedOptions{Executable: s.options.Executable, AuthRef: binding.AuthRef, AuthRoot: binding.AuthRoot, Identity: larkcli.Identity{Brand: identity.Brand, AppID: identity.AppID, UserOpenID: identity.UserOpenID}, Coordinator: s.coordinator})
}
func (s *Service) Cancel(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := s.flow(id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finished {
		return nil
	}
	_, _ = f.child.CancelAndWait()
	<-f.done
	f.cancel()
	defer f.release()
	f.finished = true
	defer close(f.finalized)
	f.finalErr = ErrNeedsReconcile
	r, err := s.readRegistry()
	if err != nil {
		return err
	}
	if i, ok := s.findEnrollment(r, f.enrollment.ProfileID); ok {
		r.Enrollments[i].State = "unknown"
		return s.saveRegistry(r)
	}
	return nil
}
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	ids := []string{}
	for id := range s.flows {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var err error
	for _, id := range ids {
		err = errors.Join(err, s.Cancel(context.Background(), id))
	}
	return err
}

// Unlink removes one product reference, never the official CLI credential. Guard
// covers all AppID references, including stale workers and pending data.
func (s *Service) Unlink(ctx context.Context, id model.ProfileID) error {
	release, err := s.lifecycleLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	b, err := s.resolve(ctx, id, true, false)
	if err != nil {
		return err
	}
	r, err := s.readRegistry()
	if err != nil {
		return err
	}
	guardRelease, err := s.guardApp(ctx, r, b.AppID)
	if err != nil {
		return err
	}
	defer guardRelease()
	if err = s.profiles.Bind(ctx, id, "", ""); err != nil {
		return err
	}
	refs := r.References[:0]
	for _, ref := range r.References {
		if ref.ConfigID != s.configID || ref.ProfileID != id {
			refs = append(refs, ref)
		}
	}
	r.References = refs
	return s.saveRegistry(r)
}

var _ larkcli.Coordinator = (*Coordinator)(nil)

func (c cliProtocol) Logout(ctx context.Context, identity larkcli.Identity) error {
	o := c.options
	r, err := larkcli.NewManaged(larkcli.ManagedOptions{Executable: o.Executable, AuthRef: o.AuthRef, AuthRoot: o.AuthRoot, Identity: identity, Coordinator: o.Coordinator})
	if err != nil {
		return err
	}
	return r.Logout(ctx)
}

// Logout invalidates every product reference BEFORE invoking the CLI. A lost
// result remains disabled and is never automatically replayed. References and
// pending data remain recoverable; only explicit same-identity reauth reenables.
func (s *Service) Logout(ctx context.Context, id model.ProfileID) error {
	release, err := s.lifecycleLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	b, err := s.resolve(ctx, id, true, false)
	if err != nil {
		return err
	}
	r, err := s.readRegistry()
	if err != nil {
		return err
	}
	for _, ref := range r.Disabled {
		if ref == b.AuthRef {
			return ErrNeedsReconcile
		}
	}
	guardRelease, err := s.guardApp(ctx, r, b.AppID)
	if err != nil {
		return err
	}
	defer guardRelease()
	if _, err = s.Status(ctx, id); err != nil {
		return err
	}
	r.Disabled = append(r.Disabled, b.AuthRef)
	if err = s.saveRegistry(r); err != nil {
		return err
	}
	driver, err := s.factory(larkcli.EnrollmentOptions{Executable: s.options.Executable, AuthRef: b.AuthRef, AuthRoot: b.AuthRoot, AppID: b.AppID, Coordinator: s.coordinator})
	if err != nil {
		return ErrNeedsReconcile
	}
	if err = driver.Logout(ctx, larkcli.Identity{Brand: b.Brand, AppID: b.AppID, UserOpenID: b.UserOpenID}); err != nil {
		return ErrNeedsReconcile
	}
	return nil
}

// Link adds a profile reference to an already verified canonical identity. It
// neither logs in again nor duplicates the CLI root; target identity changes
// require an explicit migration rather than silently adopting old pending work.
func (s *Service) Link(ctx context.Context, id model.ProfileID, authRef string) (Identity, error) {
	release, err := s.lifecycleLease(ctx)
	if err != nil {
		return Identity{}, err
	}
	defer release()
	p, err := s.profiles.Profile(ctx, id)
	if err != nil {
		return Identity{}, err
	}
	r, err := s.readRegistry()
	if err != nil {
		return Identity{}, err
	}
	var record model.IdentityRecord
	found := false
	for _, v := range r.Identities {
		if v.AuthRef == authRef {
			record = v
			found = true
			break
		}
	}
	if !found || record.Brand != p.Brand || (p.AuthRef != "" && (p.AuthRef != record.AuthRef || p.IdentityHash != record.IdentityHash)) {
		return Identity{}, model.ErrIdentityChanged
	}
	if pendingApp(r, record.AppID) {
		return Identity{}, ErrNeedsReconcile
	}
	for _, ref := range r.Disabled {
		if ref == authRef {
			return Identity{}, ErrUserMissing
		}
	}
	driver, err := s.runner(enrollment{AuthRef: authRef, AppID: record.AppID})
	if err != nil {
		return Identity{}, err
	}
	v, err := driver.VerifyIdentity(ctx)
	if err != nil {
		return Identity{}, model.ErrIdentityChanged
	}
	identity, err := s.validateIdentity(enrollment{AppID: record.AppID, Brand: record.Brand, ExpectedHash: record.IdentityHash}, v)
	if err != nil {
		return Identity{}, err
	}
	for _, ref := range r.References {
		if ref.ConfigID == s.configID && ref.ProfileID == id {
			if ref.AuthRef != record.AuthRef || ref.IdentityHash != record.IdentityHash {
				return Identity{}, model.ErrIdentityChanged
			}
			return identity, s.profiles.Bind(ctx, id, record.AuthRef, record.IdentityHash)
		}
	}
	r.References = append(r.References, Reference{ConfigID: s.configID, ConfigFile: s.options.ConfigFile, ProfileID: id, AuthRef: record.AuthRef, IdentityHash: record.IdentityHash})
	if err = s.saveRegistry(r); err != nil {
		return Identity{}, err
	}
	return identity, s.profiles.Bind(ctx, id, record.AuthRef, record.IdentityHash)
}
