package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/spf13/cobra"
)

func configFixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.StateDir = filepath.Join(root, "state")
	c.CacheDir = filepath.Join(root, "cache")
	p := model.Profile{ID: model.ProfileID(uuid.NewString()), Name: "work", Brand: "feishu"}
	r := model.Remote{ID: model.RemoteID(uuid.NewString()), Name: "team", ProfileID: p.ID, FolderToken: "synthetic_folder", NewMDPolicy: "docx"}
	mount := config.DefaultMount()
	mount.ID = model.MountID(uuid.NewString())
	mount.Name = "team"
	mount.RemoteID = r.ID
	mount.Mountpoint = filepath.Join(root, "mountpoint")
	c.Profiles = []model.Profile{p}
	c.Remotes = []model.Remote{r}
	c.Mounts = []model.Mount{mount}
	file := filepath.Join(root, "config", "bridge.json")
	if err = config.Save(file, c); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestCLIJSONNoLogPollution(t *testing.T) {
	file := configFixture(t)
	root := NewCommand(CommandOptions{})
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"--bridge-config", file, "remote", "list", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var out []model.Remote
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || len(out) != 1 {
		t.Fatalf("stdout not just JSON: %s %v", stdout.String(), err)
	}
	if stderr.Len() != 0 {
		t.Fatal(stderr.String())
	}
}
func TestReferencedRemoteRemovalHasPendingExitSix(t *testing.T) {
	file := configFixture(t)
	code := 0
	root := NewCommand(CommandOptions{Exit: func(v int) { code = v }})
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--bridge-config", file, "remote", "remove", "team", "--json"})
	if err := root.Execute(); !errors.Is(err, manager.ErrPending) {
		t.Fatal(err)
	}
	if code != 6 {
		t.Fatal(code)
	}
	var out struct {
		Error manager.Error `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || out.Error.Code != "pending" {
		t.Fatal(stdout.String(), err)
	}
	c, err := config.Load(file)
	if err != nil || len(c.Remotes) != 1 {
		t.Fatal("removed referenced folder", err)
	}
}
func TestMountAddRejectsNonFullCacheAsConfigurationError(t *testing.T) {
	file := configFixture(t)
	code := 0
	root := NewCommand(CommandOptions{Exit: func(v int) { code = v }})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--bridge-config", file, "mount", "add", "bad", "--remote", "team", "--mountpoint", filepath.Join(filepath.Dir(filepath.Dir(file)), "another-mount"), "--cache-mode", "off", "--json"})
	if err := root.Execute(); err == nil || code != 2 {
		t.Fatalf("invalid managed cache accepted/wrong code: %d %v", code, err)
	}
	c, err := config.Load(file)
	if err != nil || len(c.Mounts) != 1 {
		t.Fatal("invalid mount persisted", err)
	}
}
func TestCompletionAndHelpDoNotRequireCLIOrAccount(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"completion", "bash"}, {"completion", "zsh"}, {"completion", "fish"}, {"completion", "powershell"}} {
		root := NewCommand(CommandOptions{})
		var stdout bytes.Buffer
		root.SetOut(&stdout)
		root.SetArgs(args)
		if err := root.Execute(); err != nil || stdout.Len() == 0 {
			t.Fatal(args, err)
		}
	}
}
func TestCompletionUsesExecutableRoot(t *testing.T) {
	root := &cobra.Command{Use: "drivebridge"}
	root.AddCommand(NewCommand(CommandOptions{}))
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"feishu", "completion", "bash"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "__start_drivebridge") {
		t.Fatal("completion targets subcommand instead of executable")
	}
}
func TestLogsFollowWaitsForCancellationWithoutTrailingJSON(t *testing.T) {
	file := configFixture(t)
	root := NewCommand(CommandOptions{})
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"--bridge-config", file, "logs", "team", "--follow", "--json"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("follow returned instead of waiting for cancellation")
	}
	if stdout.Len() != 0 {
		t.Fatal("follow emitted an unrelated trailing JSON result", stdout.String())
	}
}
