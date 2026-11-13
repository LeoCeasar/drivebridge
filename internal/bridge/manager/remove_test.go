package manager

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/platform"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestDeleteMountDeletesCleanStateAndConfiguration(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := m.paths(spec.Config)
	historyMarker := filepath.Join(paths.StateDir, "history", string(spec.Profile.ID), "retained-marker")
	if err := platform.EnsurePrivateDir(filepath.Dir(historyMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(historyMarker, []byte("history is not a cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	c, err := m.LoadConfig(ctx)
	if err != nil || len(c.Mounts) != 0 {
		t.Fatalf("mount retained: %+v %v", c.Mounts, err)
	}
	for _, p := range []string{spec.Paths.Root, spec.Paths.Cache} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("cache retained: %s %v", p, err)
		}
	}
	owners, err := m.owners(ctx)
	if err != nil || len(owners) != 0 {
		t.Fatalf("dangling owners: %+v %v", owners, err)
	}
	if err := m.DeleteMount(ctx, id); err != nil {
		t.Fatalf("retry not idempotent: %v", err)
	}
	if b, err := os.ReadFile(historyMarker); err != nil || string(b) != "history is not a cache" {
		t.Fatalf("history removed: %q %v", b, err)
	}
}

func TestDeletedAccountMountIsNotRecreatedByDiscovery(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if err := m.updateConfig(ctx, func(c *config.Config) error {
		c.Remotes[0].AccountRoot = true
		c.Remotes[0].FolderToken = accountToken(c.Profiles[0].ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := m.LoadConfig(ctx)
	profile := c.Profiles[0]
	if err := m.DeleteMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensureAccountMount(ctx, profile); !errors.Is(err, ErrArchived) {
		t.Fatalf("background discovery revived deleted mapping: %v", err)
	}
	c, _ = m.LoadConfig(ctx)
	if len(c.Mounts) != 0 || !c.Remotes[0].MountRemoved {
		t.Fatal("removal intent lost")
	}
}

func TestDeleteMountRefusesOpenFiles(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.readers = 1
	runtime.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.DeleteMount(ctx, id); !errors.Is(err, ErrPending) {
		t.Fatalf("open file deleted: %v", err)
	}
	if _, err := m.ResolveMount(context.Background(), string(id)); err != nil {
		t.Fatal("configuration removed while open")
	}
}

func TestDeletedMountCleanupResumesAfterConfigWasRemoved(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	r := removalRecord{Schema: 1, Mount: id, Owners: []ownership{ownershipFor(m.ConfigFile(), spec)}, Cache: spec.Paths.Cache, Mountpoint: spec.Mount.Mountpoint}
	if err := atomicJSON(m.removalPath(id), r); err != nil {
		t.Fatal(err)
	}
	if err := m.updateConfig(ctx, func(c *config.Config) error { c.Mounts = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	m.retryRemovals(ctx)
	for _, name := range []string{spec.Paths.Root, m.removalPath(id)} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("interrupted cleanup not completed: %s %v", name, err)
		}
	}
}

func TestDeleteMountBlocksPendingDataWithoutHidingMount(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.EnsureEntry(ctx, "not-uploaded.txt", "file")
	if err != nil {
		t.Fatal(err)
	}
	e, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("keep pending"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteMount(ctx, id); !errors.Is(err, ErrMountUnsynced) {
		t.Fatalf("pending deletion accepted: %v", err)
	}
	if mount, err := m.ResolveMount(ctx, string(id)); err != nil || mount.Archived {
		t.Fatalf("pending mount hidden: %+v %v", mount, err)
	}
	f, err := s.ReadBlob(ctx, e.Local)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestDeleteUnusedMountPreservesUserMountpointContents(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Windows leaves the WinFsp target absent; create the user-owned directory.
	if err := os.MkdirAll(spec.Mount.Mountpoint, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(spec.Mount.Mountpoint, "user-data")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteMount(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "preserve" {
		t.Fatalf("user directory removed: %q %v", got, err)
	}
}

func TestDeleteLastMountAllowsInstallationLease(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	installationGates(m)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := m.WithInstallationLease(ctx, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("clean deletion blocks upgrade: called=%v err=%v", called, err)
	}
}

func TestDeleteMountAfterFailedStart(t *testing.T) {
	for _, phase := range []string{"spawn", "factory"} {
		t.Run(phase, func(t *testing.T) {
			m, id, _, _ := fixture(t, nil)
			if phase == "spawn" {
				m.spawn = func(context.Context, []string, string) (int, error) { return 0, errors.New("spawn failed") }
			} else {
				m.factory = func(context.Context, mountSpec) (*workerServices, error) { return nil, ErrDriver }
			}
			ctx := context.Background()
			if _, err := m.Start(ctx, id, true); err == nil {
				t.Fatal("expected failed start")
			}
			// Start can observe the error before the failing worker closes IPC and
			// releases its runtime ownership. Deletion is only safe after that exit.
			wait, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if err := m.waitOffline(wait, id); err != nil {
				t.Fatal(err)
			}
			spec, err := m.spec(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(filepath.Join(spec.Paths.Root, "store.db")); !os.IsNotExist(err) {
				t.Fatal("unexpected database", err)
			}
			if err := m.DeleteMount(ctx, id); err != nil {
				t.Fatalf("failed-start mapping not removable: %v", err)
			}
			if _, err := os.Stat(spec.Paths.Root); !os.IsNotExist(err) {
				t.Fatal("root retained", err)
			}
		})
	}
}

func TestDeleteMountKeepsUnknownUninitializedData(t *testing.T) {
	for _, name := range []string{"unknown.txt", "blobs/accepted", "transport/unknown.txt"} {
		t.Run(name, func(t *testing.T) {
			m, id, _, _ := fixture(t, nil)
			spec, _ := m.spec(context.Background(), id)
			file := filepath.Join(spec.Paths.Root, name)
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := m.DeleteMount(context.Background(), id); err == nil {
				t.Fatal("unknown data deleted")
			}
			if b, err := os.ReadFile(file); err != nil || string(b) != "preserve" {
				t.Fatal("data lost", err)
			}
		})
	}
}

func TestDeleteMountIgnoresStaleOfflineHistorySnapshot(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	if err := m.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(spec.Paths.Root, "history-state.json"), model.HistoryStatus{State: "pending", Pending: 51}); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status.History == nil || status.History.Pending != 0 {
		t.Fatalf("stale pending overrides store: %+v", status.History)
	}
	if err := m.DeleteMount(ctx, id); err != nil {
		t.Fatalf("stale snapshot blocked deletion: %v", err)
	}
}

func TestDeleteMountReconcilesLegacySystemMetadata(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "archived"}[archived], func(t *testing.T) {
			m, id, _, _ := fixture(t, nil)
			ctx := context.Background()
			if _, err := m.Start(ctx, id, true); err != nil {
				t.Fatal(err)
			}
			s := m.localWorker(id).services.store
			e, err := s.EnsureEntry(ctx, ".DS_Store", model.KindFile)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("legacy Finder metadata")); err != nil {
				t.Fatal(err)
			}
			if archived {
				if err := m.ArchiveMount(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.DeleteMount(ctx, id); err != nil {
				t.Fatalf("local metadata blocks removal: %v", err)
			}
			if _, err := m.ResolveMount(ctx, string(id)); !errors.Is(err, ErrConfig) {
				t.Fatal("mapping retained", err)
			}
		})
	}
}

func TestInstallationRejectsForeignConfigReference(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	if err := atomicJSON(filepath.Join(m.ownerDir(), "config.ref"), configReference{Schema: 1, ConfigFile: "/foreign/config.json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.installationConfigs(context.Background()); err == nil {
		t.Fatal("foreign domain adopted")
	}
}
