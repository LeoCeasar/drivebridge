// Package feishu adds the product CLI without changing native rclone commands.
package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/internal/bridge/buildinfo"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/spf13/cobra"
)

type CommandOptions struct {
	NewManager func(*cobra.Command) (*manager.Manager, error)
	Exit       func(int)
}

var Command = NewCommand(CommandOptions{Exit: os.Exit})

func init() { cmd.Root.AddCommand(Command) }
func flagString(c *cobra.Command, name string) string {
	for p := c; p != nil; p = p.Parent() {
		if f := p.Flags().Lookup(name); f != nil {
			return f.Value.String()
		}
		if f := p.PersistentFlags().Lookup(name); f != nil {
			return f.Value.String()
		}
	}
	return ""
}
func ManagerForCommand(c *cobra.Command) (*manager.Manager, error) {
	options := manager.Options{ConfigFile: flagString(c, config.BridgeConfigFlag), CLIExecutable: flagString(c, "lark-cli"), GitExecutable: flagString(c, "git-executable")}
	if raw := flagString(c, "wait"); raw != "" {
		duration, err := time.ParseDuration(raw)
		if err != nil || duration <= 0 {
			return nil, manager.ErrConfig
		}
		options.ReadyTimeout = duration
	}
	return manager.New(options)
}
func NewCommand(o CommandOptions) *cobra.Command {
	root := &cobra.Command{Use: "feishu", Short: "Manage personal Feishu accounts, shared folders and durable mounts", SilenceErrors: true, SilenceUsage: true}
	root.Annotations = map[string]string{"drivebridge-private-control": "true"}
	root.PersistentFlags().String(config.BridgeConfigFlag, "", "Product configuration file (separate from rclone --config)")
	root.PersistentFlags().String("lark-cli", "", "Official CLI native executable (absolute or resolved from PATH)")
	root.PersistentFlags().String("git-executable", "", "Git executable for development or diagnostics; packaged runtime is preferred")
	root.PersistentFlags().Bool("json", false, "Write structured JSON to stdout")
	root.AddCommand(&cobra.Command{Use: "edition", Short: "Show the compiled product edition without loading accounts", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		return json.NewEncoder(c.OutOrStdout()).Encode(map[string]string{"edition": buildinfo.Edition})
	}})
	installCommands(root, o)
	installMaintenanceCommands(root, o)
	installHistoryCommands(root, o)
	return root
}
func commandRun(o CommandOptions, fn func(context.Context, *manager.Manager, []string) (any, error)) func(*cobra.Command, []string) error {
	return func(c *cobra.Command, args []string) error {
		factory := o.NewManager
		if factory == nil {
			factory = ManagerForCommand
		}
		m, err := factory(c)
		var value any
		if err == nil {
			value, err = fn(c.Context(), m, args)
			closeErr := m.Close()
			if err == nil {
				err = closeErr
			}
		}
		jsonMode := flagString(c, "json") == "true"
		if err != nil {
			info := manager.ErrorInfo(err)
			if jsonMode {
				_ = json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "error": info})
			} else {
				_, _ = fmt.Fprintln(c.ErrOrStderr(), info.Message)
			}
			if o.Exit != nil {
				o.Exit(manager.ExitCode(err))
			}
			return err
		}
		if value != nil {
			encoder := json.NewEncoder(c.OutOrStdout())
			if !jsonMode {
				encoder.SetIndent("", "  ")
			}
			return encoder.Encode(value)
		}
		return nil
	}
}
