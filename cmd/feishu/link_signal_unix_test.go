//go:build unix

package feishu

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/spf13/cobra"
)

func TestLinkSIGTERMCleansProviderDescendants(t *testing.T) {
	if executable := os.Getenv("DB_LINK_SIGNAL_FIXTURE"); executable != "" {
		c := newLinkCommand(CommandOptions{NewManager: func(*cobra.Command) (*manager.Manager, error) {
			return manager.New(manager.Options{ConfigFile: configFixture(t)})
		}}, linkActions{resolve: func(ctx context.Context, _ *manager.Manager, _ string) (manager.LinkResult, error) {
			_, err := larkcli.NewDirect(executable).Run(ctx, larkcli.Request{})
			return manager.LinkResult{}, err
		}})
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		c.SetArgs([]string{"--path", "/selected/file"})
		if err := c.Execute(); err == nil {
			os.Exit(8)
		}
		os.Exit(0)
	}
	root := t.TempDir()
	pidFile := filepath.Join(root, "pids")
	script := filepath.Join(root, "provider")
	quoted := "'" + strings.ReplaceAll(pidFile, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 30 &\nprintf '%s %s' \"$$\" \"$!\" > "+quoted+"\nwait\n"), 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(self, "-test.run=^TestLinkSIGTERMCleansProviderDescendants$")
	process.Env = append(os.Environ(), "DB_LINK_SIGNAL_FIXTURE="+script)
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if process.ProcessState == nil {
			_ = process.Process.Kill()
			_ = process.Wait()
		}
	})
	var ids []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(pidFile)
		ids = strings.Fields(string(b))
		if len(ids) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(ids) != 2 {
		t.Fatal("provider did not start")
	}
	provider, _ := strconv.Atoi(ids[0])
	child, _ := strconv.Atoi(ids[1])
	defer syscall.Kill(-provider, syscall.SIGKILL)
	if err = process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = process.Wait(); err != nil {
		t.Fatalf("link command did not handle cancellation: %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && syscall.Kill(child, 0) == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(child, 0) != syscall.ESRCH {
		t.Fatal("provider descendant survived cancellation")
	}
}
