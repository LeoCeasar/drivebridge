package manager

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestDiscoveryRootAndFolderProtocol(t *testing.T) {
	root, err := decodeDiscoveryRoot([]byte(`{"id":"r","token":"rootToken","user_id":"12345"}`))
	if err != nil || root.UserID != "12345" {
		t.Fatalf("%+v %v", root, err)
	}
	for _, raw := range []string{`{"token":"root","user_id":"ou_abc"}`, `{"token":"root","user_id":12345}`, `{"token":"../root","user_id":"123"}`} {
		if _, err := decodeDiscoveryRoot([]byte(raw)); err == nil {
			t.Fatal("accepted invalid root")
		}
	}
	folder, err := decodeDiscoveryFolder([]byte(`{"token":"folderToken","name":"Canonical cloud name"}`), "folderToken")
	if err != nil || folder.Name != "Canonical cloud name" {
		t.Fatal(folder, err)
	}
	if _, err = decodeDiscoveryFolder([]byte(`{"token":"different","name":"spoof"}`), "folderToken"); err == nil {
		t.Fatal("accepted mismatched token")
	}
}
func TestDiscoveryFolderReference(t *testing.T) {
	for _, input := range []string{"folderToken", "https://team.feishu.cn/drive/folder/folderToken"} {
		got, err := discoveryFolderToken(input, "https://team.feishu.cn")
		if err != nil || got != "folderToken" {
			t.Fatal(got, err)
		}
	}
	for _, input := range []string{"../token", "https://evil.test/drive/folder/token", "https://other.feishu.cn/drive/folder/token", "https://team.feishu.cn/docx/token", "https://user@team.feishu.cn/drive/folder/token", "https://team.feishu.cn/drive/folder/token/extra"} {
		if _, err := discoveryFolderToken(input, "https://team.feishu.cn"); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
func TestDiscoveryCachedStateAndSingletonMount(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	testHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", testHome)
	t.Setenv("USERPROFILE", testHome)
	ctx := context.Background()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles[0]
	views, err := m.DiscoveryViews(ctx)
	if err != nil || len(views) != 1 || views[0].Configured {
		t.Fatalf("%+v %v", views, err)
	}
	b := discovery.Binding{ProfileID: string(p.ID), IdentityHash: p.IdentityHash, UserID: "12345", Origin: "https://team.feishu.cn"}
	state := discoverySettings{Binding: b, RootToken: "rootToken", Enabled: true, State: "complete"}
	snap := discovery.Snapshot{Binding: b, Complete: true, Entries: []discovery.Entry{{Token: "folderToken", Name: "Folder", Memberships: []string{discovery.Manual}, Manual: true}}}
	if err = m.writeDiscovery(ctx, p.ID, state, snap); err != nil {
		t.Fatal(err)
	}
	one, err := m.ensureAccountMount(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.ensureAccountMount(ctx, p)
	if err != nil || one.ID != two.ID {
		t.Fatalf("duplicate mounts: %+v %+v %v", one, two, err)
	}
	c, err = m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range c.Remotes {
		if r.AccountRoot {
			count++
			if r.FolderToken != "drivebridge-account-"+string(p.ID) {
				t.Fatal(r)
			}
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	// The existing single-folder mount remains present and keeps its identity.
	if len(c.Mounts) != 2 {
		t.Fatal(c.Mounts)
	}
	views, err = m.DiscoveryViews(ctx)
	if err != nil || !views[0].Configured || views[0].MountID != one.ID {
		t.Fatal(views, err)
	}
	spec, err := m.spec(ctx, one.ID)
	if err != nil {
		t.Fatal(err)
	}
	options, err := m.accountOptions(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := options.ReadSnapshot(ctx)
	if err != nil || len(got.Entries) != 1 {
		t.Fatal(got, err)
	}
	spec.Profile.IdentityHash = strings.Repeat("b", 64)
	if _, err = m.accountOptions(ctx, spec); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("accepted changed identity: %v", err)
	}
	root, err := m.discoveryDir(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
}

type discoveryFailureSession struct{}

func (discoveryFailureSession) Get(context.Context, string, url.Values) ([]byte, error) {
	return nil, discovery.ErrNeedsLogin
}
func (discoveryFailureSession) Login(context.Context) error     { return nil }
func (discoveryFailureSession) SealLogin(context.Context) error { return nil }
func (discoveryFailureSession) Close() error                    { return nil }
func TestDiscoveryFailedScanKeepsManualAndOldCatalog(t *testing.T) {
	b := discovery.Binding{ProfileID: "01234567-89ab-4cde-8fab-0123456789ab", IdentityHash: strings.Repeat("a", 64), UserID: "12345", Origin: "https://team.feishu.cn"}
	previous := discovery.Snapshot{Binding: b, Complete: true, Entries: []discovery.Entry{{Token: "folderToken", Name: "Old", Memberships: []string{discovery.Manual}, Manual: true}}}
	settings := discoverySettings{Binding: b, RootToken: "root", Enabled: true, State: "complete"}
	next := scanDiscoveryState(context.Background(), &settings, discoveryFailureSession{}, previous)
	if settings.State != "needs_login" || next.Complete || len(next.Entries) != 1 || !next.Entries[0].Manual {
		t.Fatalf("%+v %+v", settings, next)
	}
}

func TestDiscoveryUnconfiguredActionsRequireWebBinding(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.RescanDiscovery(context.Background(), c.Profiles[0].ID); !errors.Is(err, discovery.ErrNeedsLogin) {
		t.Fatalf("rescan fabricated binding: %v", err)
	}
	if _, err = m.AddDiscoveryFolder(context.Background(), c.Profiles[0].ID, "folderToken"); !errors.Is(err, discovery.ErrNeedsLogin) {
		t.Fatalf("manual add fabricated binding: %v", err)
	}
}
func TestDiscoverySearchMetadataAndFolderTypes(t *testing.T) {
	actual, _, _, actualErr := decodeDiscoverySearch([]byte(`{"results":[{"result_meta":{"token":"folderToken","doc_types":"FOLDER"}}],"has_more":false,"page_token":""}`), "https://team.feishu.cn")
	if actualErr != nil || len(actual) != 1 || actual[0] != "folderToken" {
		t.Fatalf("actual string doc_types form: %v %v", actual, actualErr)
	}
	got, more, cursor, err := decodeDiscoverySearch([]byte(`{"results":[{"result_meta":{"token":"folderToken","doc_types":["FOLDER"]}}],"has_more":true,"page_token":"next"}`), "https://team.feishu.cn")
	if err != nil || len(got) != 1 || got[0] != "folderToken" || !more || cursor != "next" {
		t.Fatalf("%v %v %q %v", got, more, cursor, err)
	}
	for _, raw := range []string{`{"results":[{"result_meta":{"token":"folderToken","doc_types":["DOCX"]}}],"has_more":false}`, `{"results":[{"result_meta":{"token":"folderToken","doc_types":["FOLDER"],"url":"https://evil.test/drive/folder/folderToken"}}],"has_more":false}`, `{"results":[],"has_more":true}`} {
		if _, _, _, err = decodeDiscoverySearch([]byte(raw), "https://team.feishu.cn"); err == nil {
			t.Fatal("accepted unsupported search metadata")
		}
	}
}

type discoveryTrackedSession struct {
	discoveryFailureSession
	closes int
}

func (s *discoveryTrackedSession) Close() error { s.closes++; return nil }
func TestDiscoveryBrowserSessionUsesManagerLifetime(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lifetime context.Context
	session := &discoveryTrackedSession{}
	m.discoveryFactory = func(ctx context.Context, id model.ProfileID, origin string) (discoverySession, error) {
		lifetime = ctx
		if id != c.Profiles[0].ID || origin != "https://team.feishu.cn" {
			t.Fatal("unexpected browser account binding")
		}
		return session, nil
	}
	request, cancelRequest := context.WithCancel(ctx)
	first, err := m.discoverySession(request, c.Profiles[0].ID, "https://team.feishu.cn")
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	if lifetime == nil || lifetime.Err() != nil || lifetime.Done() != m.ctx.Done() {
		t.Fatal("browser inherited request lifetime")
	}
	second, err := m.discoverySession(ctx, c.Profiles[0].ID, "https://team.feishu.cn")
	if err != nil || first != second {
		t.Fatal("duplicate browser session", err)
	}
	m.closeDiscoverySessions()
	m.closeDiscoverySessions()
	if session.closes != 1 {
		t.Fatal(session.closes)
	}
}
func TestDiscoveryOperationLockIsBoundedAndReleased(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := c.Profiles[0].ID
	release, err := m.lockDiscovery(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err = m.lockDiscovery(context.Background(), id); !errors.Is(err, ErrPending) {
		t.Fatalf("busy operation: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("lock contention unbounded")
	}
	release()
	release, err = m.lockDiscovery(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestDiscoveryMountFailurePersistsSafeStateAndCatalog(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := c.Profiles[0]
	b := discovery.Binding{ProfileID: string(profile.ID), IdentityHash: profile.IdentityHash, UserID: "12345", Origin: "https://team.feishu.cn"}
	settings := discoverySettings{Binding: b, RootToken: "rootToken", Enabled: true, State: "complete"}
	snapshot := discovery.Snapshot{Binding: b, Complete: true, Entries: []discovery.Entry{{Token: "folderToken", Name: "Retained", Manual: true, Memberships: []string{discovery.Manual}}}}
	view, err := m.failedDiscoveryCompletion(ctx, profile, settings, snapshot, ErrDriver)
	if !errors.Is(err, ErrDriver) || view.State != "unsupported" || view.Error != "unsupported" {
		t.Fatal(view, err)
	}
	stored, catalog, err := m.readDiscovery(ctx, profile.ID)
	if err != nil || stored.State != "unsupported" || len(catalog.Entries) != 1 || !catalog.Complete {
		t.Fatal(stored, catalog, err)
	}
}

func TestDiscoveryAuthorizationFailureState(t *testing.T) {
	for _, err := range []error{auth.ErrUserMissing, auth.ErrNeedsReconcile, auth.ErrMissingScopes} {
		if state := discoveryStateError(err); state != "needs_login" {
			t.Fatalf("%v mapped to %s", err, state)
		}
	}
	if state := discoveryStateError(model.ErrIdentityChanged); state != "stale" {
		t.Fatal(state)
	}
}

func TestDiscoveryExpiredSessionLoginStatePrecedesSharedIdentityGate(t *testing.T) {
	binding := discovery.Binding{ProfileID: "01234567-89ab-4cde-8fab-0123456789ab", IdentityHash: strings.Repeat("a", 64), UserID: "12345", Origin: "https://team.feishu.cn"}
	previous := discovery.Snapshot{Binding: binding, Complete: true, Sources: map[string]discovery.SourceState{discovery.Personal: {Status: "complete", Count: 1}}, Entries: []discovery.Entry{{Token: "oldFolder", Name: "Retained folder", Memberships: []string{discovery.Personal, discovery.Manual}, Manual: true}}}
	_, scanErr := discovery.Scan(context.Background(), binding, discoveryFailureSession{}, previous)
	if !errors.Is(scanErr, discovery.ErrNeedsLogin) || !errors.Is(scanErr, discovery.ErrIdentity) {
		t.Fatalf("expected login failure plus fresh-identity gate, got %v", scanErr)
	}
	settings := discoverySettings{Binding: binding, RootToken: "rootToken", State: "complete", Enabled: true}
	next := scanDiscoveryState(context.Background(), &settings, discoveryFailureSession{}, previous)
	if settings.State != "needs_login" || settings.Error != "needs_login" || next.Complete || len(next.Entries) != 1 || !next.Entries[0].Manual {
		t.Fatalf("login state overwritten: %+v %+v", settings, next)
	}
	if next.Sources[discovery.Personal].Status != "needs_login" || next.Sources[discovery.SharedVisible].Status != "partial" {
		t.Fatalf("unexpected source states: %+v", next.Sources)
	}
}

type gracefulDiscoverySession struct {
	discoveryFailureSession
	close func()
}

func (s gracefulDiscoverySession) Close() error { s.close(); return nil }
func TestManagerClosesDiscoveryBeforeCancelingBrowserLifetime(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id := c.Profiles[0].ID
	closes := 0
	m.discoverySessions = map[model.ProfileID]discoverySession{id: gracefulDiscoverySession{close: func() {
		closes++
		if m.ctx.Err() != nil {
			t.Error("browser lifetime canceled before graceful close")
		}
	}}}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if closes != 1 || m.ctx.Err() == nil {
		t.Fatal("shutdown did not close session and cancel manager")
	}
	if _, err = m.discoverySession(context.Background(), id, "https://team.feishu.cn"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("new browser accepted during shutdown: %v", err)
	}
}

func TestConcurrentManagerCloseWaitsForGracefulBrowserClose(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	finish := make(chan struct{})
	first := make(chan error, 1)
	second := make(chan error, 1)
	m.discoverySessions = map[model.ProfileID]discoverySession{c.Profiles[0].ID: gracefulDiscoverySession{close: func() { close(entered); <-finish }}}
	go func() { first <- m.Close() }()
	<-entered
	go func() { second <- m.Close() }()
	secondReturned := false
	select {
	case <-second:
		secondReturned = true
		t.Error("concurrent Close returned before graceful shutdown completed")
	case <-time.After(30 * time.Millisecond):
	}
	if m.ctx.Err() != nil {
		t.Error("concurrent Close canceled active graceful browser close")
	}
	close(finish)
	if err = <-first; err != nil {
		t.Fatal(err)
	}
	if !secondReturned {
		if err = <-second; err != nil {
			t.Fatal(err)
		}
	}
}
