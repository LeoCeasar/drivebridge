// Package desktop registers the optional user-session desktop application.
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/rclone/rclone/cmd"
	app "github.com/rclone/rclone/internal/bridge/desktop"
	"github.com/rclone/rclone/internal/bridge/desktopstartup"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/tray"
	"github.com/spf13/cobra"
)

func init() { cmd.Root.AddCommand(NewCommand()) }
func NewCommand() *cobra.Command {
	root := &cobra.Command{Use: "desktop", Short: "Run the personal background tray and open its management page", Annotations: map[string]string{"drivebridge-private-control": "true"}, SilenceUsage: true}
	var configFile, cli string
	root.PersistentFlags().StringVar(&configFile, "bridge-config", "", "Personal product configuration file")
	root.PersistentFlags().StringVar(&cli, "lark-cli", "", "Official Feishu CLI native executable")
	newManager := func() (*manager.Manager, string, error) {
		exe, err := os.Executable()
		if err != nil {
			return nil, "", err
		}
		worker, desktop := app.Executables(exe)
		// The Mac application and CLI/vendor tree are installed as one payload.
		if cli == "" && worker != exe {
			if resolved, e := manager.ResolveCLI("", worker); e == nil {
				cli = resolved
			}
		}
		if cli != "" {
			resolved, e := manager.ResolveCLI(cli, worker)
			if e != nil {
				return nil, "", e
			}
			cli = resolved
		}
		m, err := manager.New(manager.Options{ConfigFile: configFile, CLIExecutable: cli, Executable: worker})
		return m, desktop, err
	}
	output := func(c *cobra.Command, v any) error { return json.NewEncoder(c.OutOrStdout()).Encode(v) }
	root.AddCommand(&cobra.Command{Use: "capabilities", Short: "Inspect compiled desktop support without loading personal state", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return output(c, map[string]any{"schema_version": 1, "native_tray_compiled": tray.Available(), "platform": runtime.GOOS, "architecture": runtime.GOARCH})
	}})
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		m, _, err := newManager()
		if err != nil {
			return err
		}
		defer m.Close()
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		running, err := m.DesktopRunning(ctx)
		if err != nil {
			return err
		}
		return output(c, map[string]any{"schema_version": 1, "running": running, "native_tray_compiled": tray.Available(), "config_file": m.ConfigFile()})
	}}
	stop := &cobra.Command{Use: "stop", Short: "Exit the tray and management host; leave sync workers running", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		m, _, err := newManager()
		if err != nil {
			return err
		}
		defer m.Close()
		ctx, cancel := context.WithTimeout(c.Context(), 15*time.Second)
		defer cancel()
		if err = m.DesktopQuit(ctx); err != nil {
			return err
		}
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			running, e := m.DesktopRunning(ctx)
			if e != nil && !errors.Is(e, manager.ErrUnavailable) && !os.IsNotExist(e) {
				return e
			}
			if e == nil && !running {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
			}
		}
		return output(c, map[string]any{"schema_version": 1, "desktop_stopped": true, "sync_workers_preserved": true})
	}}
	var headless bool
	var wait time.Duration
	start := &cobra.Command{Use: "start", Short: "Start the background desktop and wait for its authenticated readiness", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if wait <= 0 {
			return errors.New("wait must be positive")
		}
		m, desktop, err := newManager()
		if err != nil {
			return err
		}
		defer m.Close()
		ctx, cancel := context.WithTimeout(c.Context(), wait)
		defer cancel()
		running, err := m.DesktopRunning(ctx)
		if err != nil {
			return err
		}
		if !running {
			current, _ := os.Executable()
			if desktop == current && !headless && !tray.Available() {
				return tray.ErrUnavailable
			}
			args := []string{"desktop", "_run", "--bridge-config", m.ConfigFile()}
			if cli != "" {
				resolved, e := manager.ResolveCLI(cli, current)
				if e != nil {
					return e
				}
				args = append(args, "--lark-cli", resolved)
			}
			if headless {
				args = append(args, "--headless")
			}
			state, e := platform.StateDir()
			if e != nil {
				return e
			}
			// Claim the state root before the launcher writes its first log.
			if err = m.InitializeConfig(ctx); err != nil {
				return err
			}
			if _, err = platform.SpawnDetached(ctx, desktop, args, filepath.Join(state, "logs", "desktop.stderr.log")); err != nil {
				return err
			}
		}
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			running, err = m.DesktopRunning(ctx)
			if err != nil && !errors.Is(err, manager.ErrUnavailable) && !os.IsNotExist(err) {
				return err
			}
			if running {
				break
			}
			select {
			case <-ctx.Done():
				return manager.ErrReadyTimeout
			case <-tick.C:
			}
		}
		if !headless {
			if err = m.DesktopOpen(ctx); err != nil {
				return fmt.Errorf("desktop running; open management: %w", err)
			}
		}
		return output(c, map[string]any{"schema_version": 1, "running": true, "headless_requested": headless})
	}}
	start.Flags().BoolVar(&headless, "headless", false, "Run the UI/supervisor without a native tray")
	start.Flags().DurationVar(&wait, "wait", 30*time.Second, "Maximum readiness wait")
	var runHeadless bool
	run := &cobra.Command{Use: "_run", Hidden: true, Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		m, desktop, err := newManager()
		if err != nil {
			return err
		}
		defer m.Close()
		ctx, cancel := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		running, err := m.DesktopRunning(ctx)
		if err != nil {
			return err
		}
		if running {
			if runHeadless {
				return nil
			}
			return m.DesktopOpen(ctx)
		}
		return app.Run(ctx, app.Options{Manager: m, Executable: desktop, CLIExecutable: cli, Headless: runHeadless})
	}}
	run.Flags().BoolVar(&runHeadless, "headless", false, "Run without a native tray")
	for _, enabled := range []bool{true, false} {
		enabled := enabled
		name := "disable-autostart"
		if enabled {
			name = "enable-autostart"
		}
		root.AddCommand(&cobra.Command{Use: name, Short: "Change this user's desktop login startup preference", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
			m, desktop, err := newManager()
			if err != nil {
				return err
			}
			defer m.Close()
			current, _ := os.Executable()
			if enabled && desktop == current && !tray.Available() {
				return tray.ErrUnavailable
			}
			spec := desktopstartup.Spec{Executable: desktop, ConfigFile: m.ConfigFile(), CLIExecutable: cli}
			if err = desktopstartup.Set(c.Context(), spec, enabled); err != nil {
				return err
			}
			return output(c, map[string]any{"schema_version": 1, "autostart": enabled})
		}})
	}
	root.AddCommand(start, stop, status, run)
	return root
}
