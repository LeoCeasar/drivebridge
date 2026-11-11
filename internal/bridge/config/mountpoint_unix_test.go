//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Loading config must not traverse runtime targets: an active mount can reenter
// this same loader during a directory request, and a stale mount can block it.
func TestMountpointConfigDoesNotProbeRuntimeTargets(t *testing.T) {
	for _, kind := range []string{"leaf symlink", "ancestor symlink", "ancestor file"} {
		t.Run(kind, func(t *testing.T) {
			p := testPaths(t)
			c := testConfig(p)
			target := c.Mounts[0].Mountpoint
			switch kind {
			case "leaf symlink", "ancestor symlink":
				if err := os.Symlink(filepath.Join(filepath.Dir(target), "runtime-target"), target); err != nil {
					t.Fatal(err)
				}
			case "ancestor file":
				if err := os.WriteFile(target, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "leaf symlink" {
				c.Mounts[0].Mountpoint = filepath.Join(target, "mount")
			}
			if err := SaveWithPaths(p, c); err != nil {
				t.Fatalf("configuration probed runtime target: %v", err)
			}
			if _, err := LoadWithPaths(p); err != nil {
				t.Fatalf("configuration load probed runtime target: %v", err)
			}
		})
	}
}
