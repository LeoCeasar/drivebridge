// Package desktop connects a user-session tray to the existing mount manager.
package desktop

import (
	"fmt"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
)

type Mount struct {
	ID         model.MountID `json:"id"`
	Name       string        `json:"name"`
	Mountpoint string        `json:"mountpoint"`
	Desired    bool          `json:"desired"`
}
type MountView struct {
	Mount
	Status            model.MountStatus `json:"status"`
	Error             string            `json:"error,omitempty"`
	StatusUnavailable bool              `json:"status_unavailable,omitempty"`
}

// AccountView contains cached discovery status; polling never starts a login or scan.
type AccountView struct {
	ID         model.ProfileID `json:"id"`
	Name       string          `json:"name"`
	State      string          `json:"state"`
	Configured bool            `json:"configured"`
	Enabled    bool            `json:"enabled"`
	CanRescan  bool            `json:"can_rescan"`
	LastScan   time.Time       `json:"last_scan,omitempty"`
}
type State struct {
	Accounts       []AccountView `json:"accounts,omitempty"`
	AccountError   string        `json:"account_error,omitempty"`
	ConflictCount  int           `json:"conflict_count"`
	ConflictsKnown bool          `json:"conflicts_known"`
	Kind           string        `json:"kind"`
	Text           string        `json:"text"`
	Mounts         []MountView   `json:"mounts"`
	Autostart      bool          `json:"autostart"`
	Busy           bool          `json:"busy"`
	Error          string        `json:"error,omitempty"`
}

// Aggregate only reports synced for running mounts with positively confirmed
// cloud and local state. Missing or stopped mounts remain visible in the result.
func Aggregate(mounts []MountView) State {
	out := State{Kind: "setup", Text: "尚未配置挂载", Mounts: mounts, ConflictsKnown: true}
	if len(mounts) == 0 {
		return out
	}
	priority := -1
	counts := map[string]int{}
	for _, m := range mounts {
		if m.StatusUnavailable {
			out.ConflictsKnown = false
		}
		out.ConflictCount += m.Status.Conflicts
		kind, rank := mountState(m)
		counts[kind]++
		if rank > priority {
			out.Kind, priority = kind, rank
		}
	}
	if !out.ConflictsKnown {
		out.ConflictCount = 0
	}
	labels := map[string]string{"synced": "已同步", "syncing": "正在同步", "stopped": "有挂载已停止", "starting": "正在恢复挂载", "unavailable": "状态暂不可用", "conflict": "有文件需要合并", "error": "同步需要处理", "unauthorized": "需要重新授权", "offline": "连接暂不可用"}
	out.Text = labels[out.Kind]
	if out.Kind == "conflict" && out.ConflictsKnown {
		n := 0
		for _, m := range mounts {
			n += m.Status.Conflicts
		}
		out.Text = fmt.Sprintf("%d 个冲突需要处理", n)
	}
	if counts["stopped"] == len(mounts) {
		out.Text = "同步已停止"
	}
	return out
}
func mountState(m MountView) (string, int) {
	code := m.Error
	if code == "" {
		code = m.Status.LastError
	}
	switch code {
	case "unauthorized", "identity_changed":
		return "unauthorized", 9
	case "network":
		return "offline", 7
	case "":
	default:
		return "error", 8
	}
	if m.Status.Conflicts > 0 || m.Status.CloudState == "conflict" {
		return "conflict", 8
	}
	if m.Status.CloudState == "blocked" || m.Status.CloudState == "paused" {
		return "error", 8
	}
	if !m.Status.Running {
		if m.Desired {
			return "starting", 5
		}
		return "stopped", 4
	}
	if m.Status.Queued+m.Status.Inflight+m.Status.VFSPending > 0 || !m.Status.LocalSaved {
		return "syncing", 3
	}
	switch m.Status.CloudState {
	case "synced":
		return "synced", 0
	case "queued", "merging", "uploading", "awaiting_verification", "awaiting_publish":
		return "syncing", 3
	default:
		return "unavailable", 6
	}
}
