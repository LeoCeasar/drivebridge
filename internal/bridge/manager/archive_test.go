package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

func TestArchiveMountPreservesQueuedDataAndRestoresWithoutStarting(t *testing.T) {
	ctx := context.Background()
	m, id, runtime, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	w := m.localWorker(id)
	for i := 0; i < 13; i++ {
		entry, err := w.services.store.EnsureEntry(ctx, fmt.Sprintf("preserved-%d.bin", i), model.KindFile)
		if err != nil {
			t.Fatal(err)
		}
		entry, _, err = w.services.store.Stage(ctx, entry.ID, nil, strings.NewReader("durable user content"))
		if err != nil {
			t.Fatal(err)
		}
		if i < 5 {
			if err := w.services.store.SaveConflict(ctx, model.Conflict{EntryID: entry.ID, Generation: entry.Generation, Local: entry.Local, Reason: "preserved fixture"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	before, err := m.Status(ctx, id)
	if err != nil || before.Queued != 13 || before.Conflicts != 5 {
		t.Fatal(before, err)
	}
	if err := m.ArchiveMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	unmounted := runtime.unmounted
	runtime.mu.Unlock()
	if !unmounted {
		t.Fatal("archived before safe unmount")
	}
	archived, err := m.ResolveMount(ctx, string(id))
	if err != nil || !archived.Archived || archived.Autostart {
		t.Fatal(archived, err)
	}
	status, err := m.Status(ctx, id)
	if err != nil || status.Running || status.Queued != 13 || status.Conflicts != 5 {
		t.Fatal("archive lost durable state", status, err)
	}
	if jobs, err := m.UploadStatus(ctx, id); err != nil || len(jobs) != 13 {
		t.Fatal("archive froze or lost queue", len(jobs), err)
	}
	if conflicts, err := m.Conflicts(ctx, id); err != nil || len(conflicts) != 5 {
		t.Fatal("archive hid recovery conflicts", len(conflicts), err)
	}
	if mounts, err := m.DesktopMounts(ctx); err != nil || len(mounts) != 0 {
		t.Fatal("archived mount remained in desktop recovery", mounts, err)
	}
	for _, start := range []func() error{
		func() error { _, err := m.Start(ctx, id, true); return err },
		func() error { _, err := m.StartDesktopMount(ctx, id); return err },
		func() error { return m.RunWorker(ctx, id) },
		func() error { return m.SetDesktopDesired(ctx, id, true) },
		func() error {
			return m.setAutostart(ctx, id, true, func(context.Context, platform.StartupSpec, bool) error {
				t.Fatal("archived mount changed OS startup registration")
				return nil
			})
		},
	} {
		if err := start(); !errors.Is(err, ErrArchived) {
			t.Fatal("archived mount was restarted", err)
		}
	}
	if err := m.ArchiveMount(ctx, id); err != nil {
		t.Fatal("idempotent archive", err)
	}
	if err := m.RestoreMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	restored, err := m.ResolveMount(ctx, string(id))
	if err != nil || restored.Archived || restored.Autostart || restored.ID != archived.ID || restored.Mountpoint != archived.Mountpoint || restored.RemoteID != archived.RemoteID {
		t.Fatal("restore relocated or enabled mount", restored, err)
	}
	if mounts, err := m.DesktopMounts(ctx); err != nil || len(mounts) != 1 || mounts[0].Desired {
		t.Fatal("restore automatically enabled desktop recovery", mounts, err)
	}
	status, err = m.Status(ctx, id)
	if err != nil || status.Running || status.Queued != 13 || status.Conflicts != 5 {
		t.Fatal("restore started upload or lost data", status, err)
	}
}

func TestArchiveMountRefusesOpenOrDirtyRuntime(t *testing.T) {
	for _, state := range []string{"reader", "writer", "reservation", "dirty", "unmount_error"} {
		t.Run(state, func(t *testing.T) {
			m, id, runtime, _ := fixture(t, nil)
			if _, err := m.Start(context.Background(), id, true); err != nil {
				t.Fatal(err)
			}
			runtime.mu.Lock()
			switch state {
			case "reader":
				runtime.readers = 1
			case "writer":
				runtime.writers = 1
			case "reservation":
				runtime.reservations = 1
			case "dirty":
				runtime.pending = true
			case "unmount_error":
				runtime.unmountErr = errors.New("test unmount failure")
			}
			runtime.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := m.ArchiveMount(ctx, id); !errors.Is(err, ErrPending) {
				t.Fatal("unsafe archive accepted", err)
			}
			mount, err := m.ResolveMount(context.Background(), string(id))
			if err != nil || mount.Archived {
				t.Fatal("failed stop still archived configuration", mount, err)
			}
		})
	}
}

func TestArchiveUnusedMountDoesNotCreateOrDeleteLocalData(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	spec, err := m.spec(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(spec.Mount.Mountpoint, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(spec.Mount.Mountpoint, "existing-local-data")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.ArchiveMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "preserve" {
		t.Fatal("archive removed local data", string(body), err)
	}
	if _, err := os.Stat(filepath.Join(spec.Paths.Root, "store.db")); !os.IsNotExist(err) {
		t.Fatal("archive created an unused store", err)
	}
}

func TestArchivedAccountMountCannotBeRecreatedByDiscovery(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if err := m.updateConfig(ctx, func(c *config.Config) error {
		c.Remotes[0].AccountRoot = true
		c.Remotes[0].FolderToken = accountToken(c.Profiles[0].ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := c.Profiles[0]
	if err := m.ArchiveMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensureAccountMount(ctx, profile); !errors.Is(err, ErrArchived) {
		t.Fatal("discovery revived archived account mount", err)
	}
	if err := m.RestoreMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	mount, err := m.ensureAccountMount(ctx, profile)
	if err != nil || mount.ID != id || mount.Autostart || mount.Archived {
		t.Fatal("discovery recreated restored account mount", mount, err)
	}
	c, err = m.LoadConfig(ctx)
	if err != nil || len(c.Mounts) != 1 || len(c.Remotes) != 1 {
		t.Fatal("discovery duplicated preserved configuration", c, err)
	}
}

func TestRepeatedRestoreMountPreservesNewStartIntent(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	enableFixtureDesktop(t, m)
	if err := m.ArchiveMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreMount(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if err := m.updateConfig(ctx, func(c *config.Config) error {
		c.Mounts[0].Autostart = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreMount(ctx, id); err != nil {
		t.Fatal("stale restore request", err)
	}
	mount, err := m.ResolveMount(ctx, string(id))
	if err != nil || mount.Archived || !mount.Autostart {
		t.Fatal("repeated restore disabled autostart", mount, err)
	}
	if mounts, err := m.DesktopMounts(ctx); err != nil || len(mounts) != 1 || !mounts[0].Desired {
		t.Fatal("repeated restore cleared new desktop intent", mounts, err)
	}
	if status, err := m.Status(ctx, id); err != nil || !status.Running {
		t.Fatal("repeated restore stopped restarted mount", status, err)
	}
}
