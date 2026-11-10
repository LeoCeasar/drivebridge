package manager

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	"path/filepath"
)

func (m *Manager) notifyConflict(ctx context.Context, spec mountSpec, s *store.Store, c model.Conflict) error {
	if _, err := s.ExportConflictCopies(ctx, c.ID); err != nil {
		if w := m.localWorker(spec.Mount.ID); w != nil {
			w.log("conflict_copy_failed")
		}
	}
	if !spec.Mount.Notify || !platform.DesktopAvailable() {
		return nil
	}
	claimed, err := s.ClaimConflictNotification(ctx, c.ID)
	if err != nil || !claimed {
		return err
	}
	_ = platform.Notify(ctx, "DriveBridge 同步冲突", "本地和云端修改需要合并，内容已保留。")
	args := []string{"feishu", "--bridge-config", m.options.ConfigFile}
	if m.options.CLIExecutable != "" {
		args = append(args, "--lark-cli", m.options.CLIExecutable)
	}
	args = append(args, "gui", "--conflict", string(c.ID))
	_, err = m.spawn(ctx, args, filepath.Join(spec.Paths.Logs, "conflict-ui.log"))
	return err
}
