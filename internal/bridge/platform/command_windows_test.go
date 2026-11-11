package platform

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestBackgroundUtilityKeepsPipesAndCancellationWithoutConsole(t *testing.T) {
	cmd := BackgroundCommand(context.Background(), "cmd.exe", "/d", "/c", "echo", "drivebridge-pipe")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 || cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Fatal("background utility must suppress its console without detaching pipes")
	}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != "drivebridge-pipe" {
		t.Fatalf("output pipe lost: %q %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd = BackgroundCommand(ctx, "ping.exe", "-n", "60", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("expected canceled utility to terminate: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("background utility survived cancellation")
	}
}
