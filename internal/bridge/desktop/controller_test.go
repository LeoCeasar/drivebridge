package desktop

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

type fixtureBackend struct {
	mu       sync.Mutex
	mounts   []Mount
	status   map[model.MountID]model.MountStatus
	starts   int
	startErr error
	started  chan struct{}
}

type blockedRecovery struct {
	fixtureBackend
	entered, release chan struct{}
}

func (b *blockedRecovery) Start(ctx context.Context, _ model.MountID, _ bool) error {
	select {
	case b.entered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-b.release:
		return errors.New("obsolete network failure")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestManualRetryWhileRecoveryPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &blockedRecovery{fixtureBackend: fixtureBackend{mounts: []Mount{{ID: "a", Desired: true}}, status: map[model.MountID]model.MountStatus{}}, entered: make(chan struct{}), release: make(chan struct{})}
	c := NewController(ctx, b, ControllerOptions{})
	c.Poll(ctx)
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery not entered")
	}
	if err := c.Handle(ctx, "retry", "a"); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	c.Wait()
	c.mu.Lock()
	r := c.recovery["a"]
	c.mu.Unlock()
	if r.active || r.attempts != 0 || r.code != "" {
		t.Fatalf("obsolete result overwrote manual retry: %+v", r)
	}
}

func (b *fixtureBackend) Mounts(context.Context) ([]Mount, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Mount(nil), b.mounts...), nil
}
func (b *fixtureBackend) Status(_ context.Context, id model.MountID) (model.MountStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status[id], nil
}
func (b *fixtureBackend) Start(_ context.Context, _ model.MountID, _ bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	if b.started != nil {
		b.started <- struct{}{}
	}
	return b.startErr
}
func (b *fixtureBackend) Stop(context.Context, model.MountID) error       { return nil }
func (b *fixtureBackend) Retry(context.Context, model.MountID) error      { return nil }
func (b *fixtureBackend) OpenManagement(context.Context) error            { return nil }
func (b *fixtureBackend) OpenConflicts(context.Context) error             { return nil }
func (b *fixtureBackend) OpenFolder(context.Context, model.MountID) error { return nil }
func (b *fixtureBackend) SetAutostart(context.Context, bool) error        { return nil }
func (b *fixtureBackend) Autostart(context.Context) (bool, error)         { return false, nil }

func TestRecoveryRespectsDesireAndBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &fixtureBackend{mounts: []Mount{{ID: "a", Desired: true}, {ID: "b", Desired: false}}, status: map[model.MountID]model.MountStatus{}, startErr: errors.New("offline"), started: make(chan struct{}, 4)}
	now := time.Now()
	c := NewController(ctx, b, ControllerOptions{Now: func() time.Time { return now }, ErrorCode: func(error) string { return "network" }})
	c.Poll(ctx)
	select {
	case <-b.started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	c.Wait()
	c.Poll(ctx)
	c.Wait()
	b.mu.Lock()
	n := b.starts
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("backoff ignored: %d", n)
	}
	now = now.Add(3 * time.Second)
	c.Poll(ctx)
	c.Wait()
	b.mu.Lock()
	n = b.starts
	b.mounts[0].Desired = false
	b.mu.Unlock()
	if n != 2 {
		t.Fatalf("retry missing: %d", n)
	}
	now = now.Add(time.Hour)
	c.Poll(ctx)
	c.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.starts != 2 {
		t.Fatal("stopped mount restarted")
	}
}

func TestRecoveryBoundedAndAuthNeverAutoRetried(t *testing.T) {
	for _, code := range []string{"network", "unauthorized"} {
		t.Run(code, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := &fixtureBackend{mounts: []Mount{{ID: "a", Desired: true}}, status: map[model.MountID]model.MountStatus{}, startErr: errors.New("failure")}
			now := time.Now()
			c := NewController(ctx, b, ControllerOptions{Now: func() time.Time { return now }, ErrorCode: func(error) string { return code }})
			for range 10 {
				c.Poll(ctx)
				c.Wait()
				now = now.Add(time.Hour)
			}
			b.mu.Lock()
			n := b.starts
			b.mu.Unlock()
			want := 5
			if code == "unauthorized" {
				want = 1
			}
			if n != want {
				t.Fatalf("starts=%d want %d", n, want)
			}
		})
	}
}

type accountFixtureBackend struct {
	fixtureBackend
	accounts   []AccountView
	accountErr error
	rescanned  []model.ProfileID
}

func (b *accountFixtureBackend) Accounts(context.Context) ([]AccountView, error) {
	return append([]AccountView(nil), b.accounts...), b.accountErr
}
func (b *accountFixtureBackend) RescanAccount(_ context.Context, id model.ProfileID) error {
	b.rescanned = append(b.rescanned, id)
	return nil
}
func TestOptionalAccountsPollCachedStatusAndRevalidateRescan(t *testing.T) {
	b := &accountFixtureBackend{accounts: []AccountView{{ID: "a", Name: "账号", State: "complete", Configured: true, Enabled: true, CanRescan: true}, {ID: "b", State: "needs_login"}}}
	c := NewController(context.Background(), b, ControllerOptions{})
	state := c.Poll(context.Background())
	if len(state.Accounts) != 2 || state.Accounts[0].State != "complete" || len(b.rescanned) != 0 {
		t.Fatal(state, b.rescanned)
	}
	if err := c.HandleAccount(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if err := c.HandleAccount(context.Background(), "b"); err == nil {
		t.Fatal("login-required rescan admitted")
	}
	if err := c.HandleAccount(context.Background(), "missing"); err == nil {
		t.Fatal("unknown account admitted")
	}
	b.accounts[0].CanRescan = false
	if err := c.HandleAccount(context.Background(), "a"); err == nil {
		t.Fatal("stale menu rescan admitted")
	}
	if len(b.rescanned) != 1 || b.rescanned[0] != "a" {
		t.Fatal(b.rescanned)
	}
	legacy := NewController(context.Background(), &fixtureBackend{}, ControllerOptions{})
	if err := legacy.HandleAccount(context.Background(), "a"); err == nil {
		t.Fatal("mount-only backend admitted rescan")
	}
}
func TestAccountReadFailureKeepsSyncStatusIndependent(t *testing.T) {
	b := &accountFixtureBackend{fixtureBackend: fixtureBackend{mounts: []Mount{{ID: "m"}}, status: map[model.MountID]model.MountStatus{}}, accountErr: errors.New("private detail")}
	state := NewController(context.Background(), b, ControllerOptions{}).Poll(context.Background())
	if state.AccountError != "operation_failed" || state.Kind != "stopped" {
		t.Fatal(state)
	}
}

type countFixtureBackend struct {
	fixtureBackend
	statusErr map[model.MountID]error
	mountsErr error
}

func (b *countFixtureBackend) Mounts(ctx context.Context) ([]Mount, error) {
	mounts, err := b.fixtureBackend.Mounts(ctx)
	if b.mountsErr != nil {
		err = b.mountsErr
	}
	return mounts, err
}
func (b *countFixtureBackend) Status(ctx context.Context, id model.MountID) (model.MountStatus, error) {
	s, _ := b.fixtureBackend.Status(ctx, id)
	return s, b.statusErr[id]
}
func TestTrayConflictCountsRequireEveryMountStatus(t *testing.T) {
	b := &countFixtureBackend{fixtureBackend: fixtureBackend{mounts: []Mount{{ID: "a"}, {ID: "b"}}, status: map[model.MountID]model.MountStatus{"a": {Conflicts: 2}, "b": {Conflicts: 3}}}}
	c := NewController(context.Background(), b, ControllerOptions{})
	v := trayView(c.Poll(context.Background()))
	if !v.ConflictsKnown || v.ConflictCount != 5 {
		t.Fatalf("known total: %+v", v)
	}
	b.statusErr = map[model.MountID]error{"b": errors.New("unreadable status")}
	v = trayView(c.Poll(context.Background()))
	if v.ConflictsKnown || !v.Mounts[1].StatusUnavailable {
		t.Fatalf("partial total claimed complete: %+v", v)
	}
	b.mountsErr = errors.New("unreadable config")
	if v = trayView(c.Poll(context.Background())); v.ConflictsKnown {
		t.Fatalf("failed config claimed zero: %+v", v)
	}
	b.mountsErr = nil
	b.mounts = nil
	if v = trayView(c.Poll(context.Background())); !v.ConflictsKnown || v.ConflictCount != 0 {
		t.Fatalf("empty configured mounts: %+v", v)
	}
}

func TestOpenConflictsReachesManagementConflictView(t *testing.T) {
	var opened string
	b := &managerBackend{openView: func(_ context.Context, view string) error { opened = view; return nil }}
	if err := NewController(context.Background(), b, ControllerOptions{}).Handle(context.Background(), "open_conflicts", ""); err != nil {
		t.Fatal(err)
	}
	if opened != "conflicts" {
		t.Fatalf("opened view %q", opened)
	}
}

type globalCountBackend struct {
	fixtureBackend
	count     int
	err       error
	statusErr error
}

func (b *globalCountBackend) Status(ctx context.Context, id model.MountID) (model.MountStatus, error) {
	status, _ := b.fixtureBackend.Status(ctx, id)
	return status, b.statusErr
}
func (b *globalCountBackend) ConflictCount(context.Context) (int, error) { return b.count, b.err }
func TestGlobalConflictCountOverridesActiveMountSubtotal(t *testing.T) {
	b := &globalCountBackend{fixtureBackend: fixtureBackend{mounts: []Mount{{ID: "active"}}, status: map[model.MountID]model.MountStatus{"active": {Conflicts: 2}}}, count: 5}
	c := NewController(context.Background(), b, ControllerOptions{})
	v := trayView(c.Poll(context.Background()))
	if !v.ConflictsKnown || v.ConflictCount != 5 || v.Text != "5 个冲突需要处理" {
		t.Fatalf("archived conflict count lost or inconsistent: %+v", v)
	}
	b.err = errors.New("archived state unreadable")
	v = trayView(c.Poll(context.Background()))
	if v.ConflictsKnown {
		t.Fatalf("unreadable archived mount reported complete: %+v", v)
	}
	b.err = nil
	b.mounts = []Mount{{ID: "active"}}
	b.statusErr = errors.New("active state unreadable")
	v = trayView(c.Poll(context.Background()))
	if v.ConflictsKnown {
		t.Fatalf("failed active read hidden by global count: %+v", v)
	}
}
func TestConfiguredConflictCountIncludesArchivedMountsAndRejectsPartialReads(t *testing.T) {
	mounts := []model.Mount{{ID: "active"}, {ID: "archived", Archived: true}}
	b := &countFixtureBackend{fixtureBackend: fixtureBackend{status: map[model.MountID]model.MountStatus{"active": {Conflicts: 2}, "archived": {Conflicts: 5}}}}
	count, err := configuredConflictCount(context.Background(), mounts, b)
	if err != nil || count != 7 {
		t.Fatalf("global count %d: %v", count, err)
	}
	b.statusErr = map[model.MountID]error{"archived": errors.New("state unreadable")}
	if count, err = configuredConflictCount(context.Background(), mounts, b); err == nil || count != 0 {
		t.Fatalf("partial count %d: %v", count, err)
	}
}
