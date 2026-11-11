//go:build !windows

package manager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkerRechecksMountpointBeforeBuildingRuntime(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := c.Mounts[0].Mountpoint
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(filepath.Dir(target), "missing-runtime-target"), target); err != nil {
		t.Fatal(err)
	}
	built := false
	m.factory = func(context.Context, mountSpec) (*workerServices, error) {
		built = true
		return nil, errors.New("runtime must not be built for an alias")
	}
	if err = m.RunWorker(context.Background(), id); err == nil || built {
		t.Fatalf("worker bypassed target preflight: built=%v err=%v", built, err)
	}
}

func TestMountpointStartupRejectsSymlinks(t *testing.T) {
	for _, child := range []string{"", "missing-child"} {
		t.Run(child, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "target")
			if err = os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "alias")
			if err = os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			if err = validateMountpoint(filepath.Join(alias, child)); err == nil {
				t.Fatal("startup accepted mountpoint symlink")
			}
			if _, err = os.Stat(filepath.Join(target, "missing-child")); !os.IsNotExist(err) {
				t.Fatalf("startup created a directory through the alias: %v", err)
			}
		})
	}
}

func TestMountpointStartupChecksTargetAvailability(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "mount")
	if err = validateMountpoint(target); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(target, "existing-file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = validateMountpoint(target); !errors.Is(err, ErrPending) {
		t.Fatalf("occupied mountpoint accepted: %v", err)
	}
}
