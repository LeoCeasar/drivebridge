package config

import (
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestAccountDirectoryConfiguration(t *testing.T) {
	p := testPaths(t)
	base := func() Config {
		c := testConfig(p)
		c.Remotes[0].AccountRoot = true
		c.Remotes[0].FolderToken = "drivebridge-account-" + string(c.Profiles[0].ID)
		return c
	}
	c := base()
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWithPaths(p)
	if err != nil || !loaded.Remotes[0].AccountRoot {
		t.Fatalf("account mode round trip: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"wrong account binding", func(c *Config) { c.Remotes[0].FolderToken += "-other" }},
		{"reserved legacy token", func(c *Config) { c.Remotes[0].AccountRoot = false }},
		{"duplicate account", func(c *Config) {
			r := c.Remotes[0]
			r.ID = model.RemoteID(identityID)
			c.Remotes = append(c.Remotes, r)
		}},
		{"duplicate writable mount", func(c *Config) {
			m := c.Mounts[0]
			m.ID = model.MountID(identityID)
			m.Mountpoint = filepath.Join(filepath.Dir(p.StateDir), "other-mount")
			c.Mounts = append(c.Mounts, m)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.change(&c)
			if err := Validate(c, p); err == nil {
				t.Fatal("unsafe account configuration accepted")
			}
		})
	}
}
