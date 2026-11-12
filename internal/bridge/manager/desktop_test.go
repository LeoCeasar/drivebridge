package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

func enableFixtureDesktop(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.enableDesktop(context.Background(), func(context.Context, platform.StartupSpec, bool) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// Losing the persisted false intent would allow a supervisor to undo CLI stop.
func TestDesktopIntentPersistsAndStopPreventsRecovery(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	mounts, err := m.DesktopMounts(ctx)
	if err != nil || len(mounts) != 1 || mounts[0].Desired {
		t.Fatalf("initial intent: %+v %v", mounts, err)
	}
	if _, err = m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err = m.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	other, err := newManager(m.options, dependencies{domain: m.domain, startLease: m.startLease, lifecycleLease: m.lifecycleLease})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mounts, err = other.DesktopMounts(ctx)
	if err != nil || mounts[0].Desired {
		t.Fatalf("stop lost after reload: %+v %v", mounts, err)
	}
	other.spawn = func(context.Context, []string, string) (int, error) {
		t.Error("stopped mount respawned")
		return 0, ErrConfig
	}
	state, err := other.StartDesktopMount(ctx, id)
	if err != nil || state.Running {
		t.Fatalf("automatic start ignored stop: %+v %v", state, err)
	}
}

// A failed migration must retain both initial intent and pending registrations.
func TestDesktopMigrationRetainsIntentUntilLegacyDisabled(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if err := m.updateConfig(ctx, func(c *config.Config) error { c.Mounts[0].Autostart = true; return nil }); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("registration unavailable")
	err := m.enableDesktop(ctx, func(_ context.Context, spec platform.StartupSpec, enabled bool) error {
		if spec.ID != string(id) || enabled {
			t.Fatalf("unsafe migration: %+v %v", spec, enabled)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(ctx)
	if err != nil || !mounts[0].Desired {
		t.Fatalf("migration lost intent: %+v %v", mounts, err)
	}
	enableFixtureDesktop(t, m)
	if err = m.SetDesktopDesired(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	enableFixtureDesktop(t, m)
	mounts, err = m.DesktopMounts(ctx)
	if err != nil || mounts[0].Desired {
		t.Fatalf("re-enable reset stop: %+v %v", mounts, err)
	}
}

func TestDesktopUnreadableStateDoesNotStart(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	path := m.ConfigFile() + ".desktop.json"
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	m.spawn = func(context.Context, []string, string) (int, error) {
		t.Error("spawn after persistence failure")
		return 0, nil
	}
	if _, err := m.Start(context.Background(), id, true); err == nil {
		t.Fatal("accepted corrupt desktop state")
	}
}

func TestDesktopMaintenanceStopPreservesIntent(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.stopForRecovery(context.Background(), id, time.Second); err != nil {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(context.Background())
	if err != nil || !mounts[0].Desired {
		t.Fatalf("maintenance cleared desired: %+v %v", mounts, err)
	}
}

func TestDesktopStopSerializesWithAutomaticStart(t *testing.T) {
	gate := make(chan struct{})
	m, id, _, _ := fixture(t, gate)
	enableFixtureDesktop(t, m)
	if err := m.SetDesktopDesired(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { _, err := m.StartDesktopMount(context.Background(), id); started <- err }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.waitEndpoint(ctx, id); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- m.Stop(context.Background(), id, time.Second) }()
	close(gate)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(ctx)
	if err != nil || mounts[0].Desired {
		t.Fatalf("stop raced away: %+v %v", mounts, err)
	}
	state, err := m.StartDesktopMount(ctx, id)
	if err != nil || state.Running {
		t.Fatalf("recovered stopped worker: %+v %v", state, err)
	}
}

func TestDesktopEnableRejectsSymlinkState(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, m.ConfigFile()+".desktop.json"); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires Windows privileges: %v", err)
		}
		t.Fatal(err)
	}
	if err := m.enableDesktop(context.Background(), func(context.Context, platform.StartupSpec, bool) error { return nil }); err == nil {
		t.Fatal("accepted symlink state")
	}
	raw, err := os.ReadFile(outside)
	if err != nil || string(raw) != "keep" {
		t.Fatalf("changed symlink target: %q %v", raw, err)
	}
}

// StopAll must disable recovery even for a worker that is already offline.
func TestDesktopStopAllClearsOfflineIntent(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if err := m.SetDesktopDesired(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.StopAll(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(context.Background())
	if err != nil || mounts[0].Desired {
		t.Fatalf("offline stop-all kept recovery enabled: %+v %v", mounts, err)
	}
}

// Legacy CLI autostart changes must not register duplicate per-mount services
// once desktop mode owns recovery.
func TestDesktopSetAutostartChangesIntent(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if err := m.setAutostart(context.Background(), id, true, func(context.Context, platform.StartupSpec, bool) error {
		return errors.New("unexpected legacy registration")
	}); err != nil {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(context.Background())
	if err != nil || !mounts[0].Desired {
		t.Fatalf("autostart ignored desktop intent: %+v %v", mounts, err)
	}
}

func TestDesktopFailedStopStillDisablesRecovery(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.pending = true
	runtime.mu.Unlock()
	if err := m.Stop(context.Background(), id, 0); !errors.Is(err, ErrPending) {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(context.Background())
	if err != nil || mounts[0].Desired {
		t.Fatalf("failed stop kept recovery enabled: %+v %v", mounts, err)
	}
}

func TestDesktopAutomaticStartRespectsInstallationAdmission(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if err := m.SetDesktopDesired(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	release, err := m.lifecycleLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	m.spawn = func(context.Context, []string, string) (int, error) {
		t.Error("automatic start bypassed installation lease")
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.StartDesktopMount(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("start admission: %v", err)
	}
}

func TestDesktopQuiescencePreservesIntent(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.WithQuiescence(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mounts, err := m.DesktopMounts(ctx)
	if err != nil || !mounts[0].Desired {
		t.Fatalf("maintenance cleared desired: %+v %v", mounts, err)
	}
}

// Partial migration must not repeat a successfully removed OS registration on
// retry: some platform removal commands report missing registrations as errors.
func TestDesktopMigrationRecordsEachLegacyRemoval(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	second := model.MountID(uuid.NewString())
	if err := m.updateConfig(ctx, func(c *config.Config) error {
		c.Mounts[0].Autostart = true
		mount := c.Mounts[0]
		mount.ID, mount.Name, mount.Mountpoint = second, "second", filepath.Join(filepath.Dir(mount.Mountpoint), "second")
		c.Mounts = append(c.Mounts, mount)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("second registration unavailable")
	if err := m.enableDesktop(ctx, func(_ context.Context, spec platform.StartupSpec, _ bool) error {
		if spec.ID == string(second) {
			return sentinel
		}
		return nil
	}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := m.enableDesktop(ctx, func(_ context.Context, spec platform.StartupSpec, _ bool) error {
		if spec.ID == string(id) {
			return errors.New("repeated removal of completed registration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopMigrationReportsIntentWriteFailure(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	if err := m.updateConfig(context.Background(), func(c *config.Config) error { c.Mounts[0].Autostart = true; return nil }); err != nil {
		t.Fatal(err)
	}
	err := m.enableDesktop(context.Background(), func(context.Context, platform.StartupSpec, bool) error {
		// Change the target after the initial durable save to force the completion
		// write itself to fail, while avoiding permissions changed by private locks.
		path := m.ConfigFile() + ".desktop.json"
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.Mkdir(path, 0700)
	})
	if err == nil {
		t.Fatal("reported migration complete after failed intent write")
	}
}
