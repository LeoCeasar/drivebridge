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

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
)

func hiddenDiscoveryFixture(t *testing.T, m *Manager, p model.Profile, token string) discovery.Snapshot {
	t.Helper()
	b := discovery.Binding{ProfileID: string(p.ID), IdentityHash: p.IdentityHash, UserID: "12345", Origin: "https://team.feishu.cn"}
	snap := discovery.Snapshot{Binding: b, Complete: true, Entries: []discovery.Entry{{Token: token, Name: "Hidden folder", Memberships: []string{discovery.SharedHidden}}}, Sources: map[string]discovery.SourceState{discovery.SharedHidden: {Status: "complete", Count: 1, Pages: 1}}}
	if err := m.writeDiscovery(context.Background(), p.ID, discoverySettings{Binding: b, RootToken: "personal-root", Enabled: true, State: "complete"}, snap); err != nil {
		t.Fatal(err)
	}
	return snap
}
func TestHiddenSharedPreferenceDefaultsPersistsAndIsolatesAccounts(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	one := c.Profiles[0]
	two := one
	two.ID = model.ProfileID(uuid.NewString())
	two.Name = "second"
	two.IdentityHash = strings.Repeat("b", 64)
	two.AuthRef = "auth/identities/" + uuid.NewString() + "/lark"
	if err = m.updateConfig(ctx, func(c *config.Config) error { c.Profiles = append(c.Profiles, two); return nil }); err != nil {
		t.Fatal(err)
	}
	hiddenDiscoveryFixture(t, m, one, "hidden-one")
	hiddenDiscoveryFixture(t, m, two, "hidden-two")
	before, err := m.discoveryView(ctx, one)
	if err != nil || !before.SyncHiddenShared {
		t.Fatal("old settings not enabled", before, err)
	}
	root, _ := m.discoveryDir(ctx, one.ID)
	rawBefore, _ := os.ReadFile(filepath.Join(root, "snapshot.json"))
	view, err := m.SetHiddenSharedSync(ctx, one.ID, false)
	if err != nil || view.SyncHiddenShared || view.Snapshot.Sources[discovery.SharedHidden].Status != "disabled" {
		t.Fatal(view, err)
	}
	settings, snap, err := m.readDiscovery(ctx, one.ID)
	if err != nil || settings.SyncHiddenShared == nil || *settings.SyncHiddenShared || len(snap.Entries) != 1 || snap.Sources[discovery.SharedHidden].Status != "complete" {
		t.Fatal("saved catalog/policy wrong", settings, snap, err)
	}
	rawAfter, _ := os.ReadFile(filepath.Join(root, "snapshot.json"))
	if string(rawBefore) != string(rawAfter) {
		t.Fatal("preference rewrote catalog")
	}
	other, err := m.discoveryView(ctx, two)
	if err != nil || !other.SyncHiddenShared || other.Snapshot.Sources[discovery.SharedHidden].Status != "complete" {
		t.Fatal("other account affected", other, err)
	}
	enabled, err := m.SetHiddenSharedSync(ctx, one.ID, true)
	if err != nil || !enabled.SyncHiddenShared || enabled.Snapshot.Sources[discovery.SharedHidden].Status != "complete" {
		t.Fatal(enabled, err)
	}
}
func TestHiddenSharedSettingDoesNotStartStoppedOrArchivedAccountMount(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	testHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", testHome)
	t.Setenv("USERPROFILE", testHome)
	c, _ := m.LoadConfig(ctx)
	p := c.Profiles[0]
	hiddenDiscoveryFixture(t, m, p, "hidden")
	mount, err := m.ensureAccountMount(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.writeDesktopState(&desktopState{SchemaVersion: 1, ConfigFile: m.ConfigFile(), Desired: map[model.MountID]bool{mount.ID: false}}); err != nil {
		t.Fatal(err)
	}
	// A failed local invalidation is not a failed preference save. This also
	// proves the setter never replaces the stopped worker to recover its IPC.
	stoppedSpec, err := m.spec(ctx, mount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(stoppedSpec.Paths.Root, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(stoppedSpec.Paths.Root, "ipc.json"), []byte("invalid stale endpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	starts := 0
	m.spawn = func(context.Context, []string, string) (int, error) { starts++; return 0, errors.New("must not start") }
	for _, archived := range []bool{false, true} {
		if err = m.updateConfig(ctx, func(c *config.Config) error {
			for i := range c.Mounts {
				if c.Mounts[i].ID == mount.ID {
					c.Mounts[i].Archived = archived
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err = m.SetHiddenSharedSync(ctx, p.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 0 {
		t.Fatal("stopped mount started")
	}
	intent, err := m.readDesktopState()
	if err != nil || intent.Desired[mount.ID] {
		t.Fatal("start intent changed", intent, err)
	}
	spec, err := m.spec(ctx, mount.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(spec.Paths.Root, "store.db")); !os.IsNotExist(err) {
		t.Fatal("policy created a worker Store", err)
	}
}
func TestHiddenSharedCallbacksReadLivePolicyAndRejectReboundIdentity(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	c, _ := m.LoadConfig(ctx)
	p := c.Profiles[0]
	hiddenDiscoveryFixture(t, m, p, "hidden")
	mount, err := m.ensureAccountMount(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := m.spec(ctx, mount.ID)
	if err != nil {
		t.Fatal(err)
	}
	options, err := m.accountOptions(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	value, err := options.SyncHiddenShared(ctx)
	if err != nil || !value {
		t.Fatal(value, err)
	}
	if _, err = m.SetHiddenSharedSync(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	value, err = options.SyncHiddenShared(ctx)
	if err != nil || value {
		t.Fatal("worker policy snapshot stayed stale", value, err)
	}
	settings, snapshot, _ := m.readDiscovery(ctx, p.ID)
	settings.Binding.IdentityHash = strings.Repeat("b", 64)
	snapshot.Binding = settings.Binding
	if err = m.writeDiscovery(ctx, p.ID, settings, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err = options.SyncHiddenShared(ctx); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("rebound preference accepted", err)
	}
	if _, err = m.SetHiddenSharedSync(ctx, p.ID, true); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("rebound save accepted", err)
	}
}
func TestHiddenSharedUnconfiguredPreferenceRequiresExistingBinding(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	c, _ := m.LoadConfig(ctx)
	if _, err := m.SetHiddenSharedSync(ctx, c.Profiles[0].ID, false); !errors.Is(err, discovery.ErrNeedsLogin) {
		t.Fatal(err)
	}
}

type hiddenScanSession struct {
	discoveryFailureSession
	hiddenCalls int
}

func (s *hiddenScanSession) Get(_ context.Context, p string, q url.Values) ([]byte, error) {
	if q.Get("hidden") == "1" {
		s.hiddenCalls++
		return nil, discovery.ErrNeedsLogin
	}
	if strings.Contains(p, "my_space/folder") {
		return []byte(`{"code":0,"data":{"entities":{"nodes":{"personal":{"token":"personal","obj_token":"personal","name":"Personal","owner_id":"12345","type":0}}},"node_list":["personal"],"has_more":false,"last_label":""}}`), nil
	}
	return []byte(`{"code":0,"data":{"entities":{"nodes":{}},"node_list":[],"has_more":false,"last_label":""}}`), nil
}
func TestHiddenSharedScanPreferenceSkipsDisabledSourceAndKeepsRawEntries(t *testing.T) {
	disabled := false
	b := discovery.Binding{ProfileID: "profile", IdentityHash: "identity", UserID: "12345", Origin: "https://team.feishu.cn"}
	settings := discoverySettings{Binding: b, SyncHiddenShared: &disabled, Enabled: true}
	old := discovery.Snapshot{Binding: b, Complete: true, Entries: []discovery.Entry{{Token: "hidden", Name: "Old hidden", Memberships: []string{discovery.SharedHidden}}}}
	session := &hiddenScanSession{}
	next := scanDiscoveryState(context.Background(), &settings, session, old)
	if session.hiddenCalls != 0 || !next.Complete || settings.State != "complete" || next.Sources[discovery.SharedHidden].Status != "disabled" || len(next.Entries) != 2 {
		t.Fatal(settings, next, session.hiddenCalls)
	}
}

func TestHiddenSharedPreferenceUsesDiscoveryOperationLock(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	ctx := context.Background()
	c, _ := m.LoadConfig(ctx)
	p := c.Profiles[0]
	hiddenDiscoveryFixture(t, m, p, "hidden")
	unlock, err := m.lockDiscovery(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	limited, stop := context.WithTimeout(ctx, 75*time.Millisecond)
	_, err = m.SetHiddenSharedSync(limited, p.ID, false)
	stop()
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("save bypassed scan lock", err)
	}
	settings, _, err := m.readDiscovery(ctx, p.ID)
	if err != nil || settings.SyncHiddenShared != nil {
		t.Fatal("failed save changed preference", settings, err)
	}
	if _, err = m.SetHiddenSharedSync(ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
}
