package feishu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/ui"
	"github.com/spf13/cobra"
)

var guiNoOpenBrowser bool
var guiConflict string
var guiCommand = &cobra.Command{Use: "gui", Short: "打开当前用户的文件与冲突管理界面", Args: cobra.NoArgs, RunE: runGUI}

func init() {
	guiCommand.Flags().BoolVar(&guiNoOpenBrowser, "no-open-browser", false, "只输出本机管理地址，不自动打开浏览器")
	guiCommand.Flags().StringVar(&guiConflict, "conflict", "", "打开指定冲突的本机合并页")
	Command.AddCommand(guiCommand)
}
func runGUI(command *cobra.Command, _ []string) error {
	m, err := ManagerForCommand(command)
	if err != nil {
		return err
	}
	defer m.Close()
	backend, err := ui.NewManagerBackend(m)
	if err != nil {
		return err
	}
	state, err := platform.StateDir()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(m.ConfigFile())))
	key := hex.EncodeToString(sum[:])
	ctx, cancel := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	runtimeDir := filepath.Join(state, "ui")
	options := ui.Options{Backend: backend, RuntimeDir: runtimeDir, ConfigKey: key}
	if guiNoOpenBrowser {
		options.Notifier = quietGUI{}
	}
	host, err := ui.Start(ctx, options)
	var address string
	if errors.Is(err, ui.ErrAlreadyRunning) {
		address, err = ui.OpenExisting(ctx, runtimeDir, key, model.ConflictID(guiConflict))
		if err != nil {
			return err
		}
		fmt.Fprintln(command.OutOrStdout(), address)
		if !guiNoOpenBrowser && platform.DesktopAvailable() {
			return platform.OpenBrowser(ctx, address)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer host.Close(context.Background())
	address, err = host.BootstrapURL(model.ConflictID(guiConflict))
	if err != nil {
		return err
	}
	fmt.Fprintln(command.OutOrStdout(), address)
	if !guiNoOpenBrowser && platform.DesktopAvailable() {
		if err = platform.OpenBrowser(ctx, address); err != nil {
			fmt.Fprintln(command.ErrOrStderr(), "浏览器未打开，请使用上方本机地址。")
		}
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = host.PollConflicts(ctx)
		}
	}
}

type quietGUI struct{}

func (quietGUI) Available() bool                              { return false }
func (quietGUI) Notify(context.Context, string, string) error { return nil }
func (quietGUI) Open(context.Context, string) error           { return nil }
