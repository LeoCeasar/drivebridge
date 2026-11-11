// Package tray adapts desktop state to an optional native system tray.
package tray

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnavailable = errors.New("native tray unavailable (build with -tags desktop; macOS/Linux also require CGO_ENABLED=1)")
	ErrNoDisplay   = errors.New("native tray requires a working graphical display")
)

type View struct {
	State, Text     string
	Accounts        []Account
	AccountError    string
	Mounts          []Mount
	Autostart, Busy bool
}

type Mount struct {
	ID, Name, Mountpoint, State string
	Running, Desired            bool
	Pending, Conflicts          int
	Error                       string
}

type Account struct {
	ID, Name, State                string
	Configured, Enabled, CanRescan bool
	LastScan                       time.Time
}

type Action struct{ Kind, MountID, ProfileID string }

type menuEntry struct {
	Key, Parent, Label, Tooltip string
	Enabled                     bool
	Action                      Action
}

func stateLabel(state string) string {
	switch state {
	case "ready":
		return "就绪"
	case "syncing":
		return "同步中"
	case "conflict":
		return "有冲突"
	case "auth", "error":
		return "授权或错误"
	case "stopped":
		return "已停止"
	default:
		return "不可用"
	}
}

func menu(v View) []menuEntry {
	text := strings.TrimSpace(v.Text)
	if text == "" {
		text = stateLabel(v.State)
	}
	entries := []menuEntry{
		{Key: "status", Label: "DriveBridge · " + text},
		{Key: "open", Label: "打开管理页", Enabled: true, Action: Action{Kind: "open_manager"}},
		{Key: "mounts", Label: "挂载", Enabled: len(v.Mounts) > 0},
	}
	if len(v.Accounts) > 0 || v.AccountError != "" {
		entries = append(entries, menuEntry{Key: "accounts", Label: "账号与目录发现", Enabled: true})
		if v.AccountError != "" {
			entries = append(entries, menuEntry{Key: "accounts:error", Parent: "accounts", Label: "目录发现状态暂不可用"})
		}
	}
	for _, account := range v.Accounts {
		parent := "account:" + base64.RawURLEncoding.EncodeToString([]byte(account.ID))
		name := strings.TrimSpace(account.Name)
		if name == "" {
			name = account.ID
		}
		lastScan := "尚未扫描"
		if !account.LastScan.IsZero() {
			lastScan = account.LastScan.Local().Format("01-02 15:04")
		}
		entries = append(entries,
			menuEntry{Key: parent, Parent: "accounts", Label: name, Enabled: true},
			menuEntry{Key: parent + ":login", Parent: parent, Label: accountLoginLabel(account)},
			menuEntry{Key: parent + ":status", Parent: parent, Label: discoveryLabel(account.State)},
			menuEntry{Key: parent + ":last_scan", Parent: parent, Label: "最近扫描：" + lastScan},
			menuEntry{Key: parent + ":rescan", Parent: parent, Label: "重新扫描目录", Enabled: !v.Busy && account.CanRescan, Action: Action{Kind: "rescan_discovery", ProfileID: account.ID}},
			menuEntry{Key: parent + ":manage", Parent: parent, Label: "管理账号与登录窗口", Enabled: true, Action: Action{Kind: "open_manager"}},
		)
	}
	for _, m := range v.Mounts {
		// Keys contain the ID, never the display name or a filesystem path.
		parent := "mount:" + base64.RawURLEncoding.EncodeToString([]byte(m.ID))
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = m.ID
		}
		entries = append(entries,
			menuEntry{Key: parent, Parent: "mounts", Label: name + " · " + stateLabel(m.State), Tooltip: m.Error, Enabled: true},
			menuEntry{Key: parent + ":status", Parent: parent, Label: fmt.Sprintf("待处理 %d · 冲突 %d", m.Pending, m.Conflicts), Tooltip: m.Error},
		)
		if m.Error != "" {
			entries = append(entries, menuEntry{Key: parent + ":error", Parent: parent, Label: "错误：" + m.Error})
		}
		for _, e := range []struct {
			kind, label string
			enabled     bool
		}{
			{"open_folder", "打开本地目录", m.Mountpoint != ""},
			{"start", "启动同步", !v.Busy && !m.Running},
			{"stop", "停止同步", !v.Busy && (m.Running || m.Desired)},
			{"retry", "重试", !v.Busy},
		} {
			entries = append(entries, menuEntry{Key: parent + ":" + e.kind, Parent: parent, Label: e.label, Enabled: e.enabled, Action: Action{Kind: e.kind, MountID: m.ID}})
		}
	}
	kind, label := "enable_autostart", "启用登录时启动"
	if v.Autostart {
		kind, label = "disable_autostart", "停用登录时启动"
	}
	entries = append(entries,
		menuEntry{Key: "autostart", Label: label, Enabled: !v.Busy, Action: Action{Kind: kind}},
		menuEntry{Key: "stop_all", Label: "停止全部同步", Enabled: !v.Busy && len(v.Mounts) > 0, Action: Action{Kind: "stop_all"}},
		menuEntry{Key: "quit", Label: "退出托盘（继续同步）", Enabled: true, Action: Action{Kind: "quit"}},
	)
	return entries
}

// serve owns the menu model and queues actions without stalling state updates.
// Native callbacks only post keys. A removed or disabled item cannot dispatch.
func serve(ctx context.Context, initial View, updates <-chan View, clicks <-chan string, actions chan<- Action, render func(View, []menuEntry)) error {
	current := map[string]menuEntry{}
	apply := func(v View) {
		entries := menu(v)
		clear(current)
		for _, e := range entries {
			current[e.Key] = e
		}
		render(v, entries)
	}
	apply(initial)
	var pending []Action
	for {
		var out chan<- Action
		var next Action
		if len(pending) != 0 {
			out, next = actions, pending[0]
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case v, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			apply(v)
		case key, ok := <-clicks:
			if !ok {
				clicks = nil
				continue
			}
			e, ok := current[key]
			if !ok || !e.Enabled || e.Action.Kind == "" {
				continue
			}
			duplicate := false
			for _, a := range pending {
				if a == e.Action {
					duplicate = true
					break
				}
			}
			if !duplicate {
				pending = append(pending, e.Action)
			}
		case out <- next:
			pending = pending[1:]
		}
	}
}

func accountLoginLabel(account Account) string {
	switch account.State {
	case "needs_login":
		return "应用登录：需要重新登录"
	case "login_pending":
		return "应用登录：等待完成"
	case "needs_runtime":
		return "应用登录：需要安装 WebView2 Runtime"
	case "needs_browser":
		return "应用登录：登录组件暂不可用"
	case "needs_extension":
		return "应用登录：需要重新登录"
	case "browser_disconnected":
		return "应用登录：需要重新登录，已有同步继续"
	case "partial", "stale":
		return "应用登录：会话状态待核验"
	case "idle", "scanning":
		return "应用登录：等待目录核验"
	case "unsupported":
		return "应用登录：当前平台暂不支持"
	}
	if !account.Configured {
		return "应用登录：尚未设置"
	}
	return "应用登录：已保存"
}
func discoveryLabel(state string) string {
	switch state {
	case "complete":
		return "目录发现：本次扫描完成"
	case "partial":
		return "目录发现：部分完成"
	case "stale":
		return "目录发现：保留旧清单"
	case "scanning":
		return "目录发现：正在扫描"
	case "disabled":
		return "目录发现：未启用"
	case "unsupported":
		return "目录发现：来源暂不支持"
	case "unavailable":
		return "目录发现：暂不可用"
	case "needs_extension", "browser_disconnected":
		return "目录发现：等待重新登录"
	case "needs_runtime":
		return "目录发现：等待安装 WebView2 Runtime"
	case "needs_login", "login_pending", "needs_browser", "unconfigured":
		return "目录发现：等待应用登录"
	default:
		return "目录发现：等待扫描"
	}
}

// Both normal Quit and the platform's exit callback can request shutdown.
func shutdownOnce(cleanup func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if cleanup != nil {
				cleanup()
			}
		})
	}
}

// App cancellation starts cleanup while the native menu remains alive. Native
// Quit is admitted only after cleanup has finished; its own exit callback uses
// the same gate when the platform initiates termination first.
func nativeLifetime(ctx context.Context, cleanup func()) (context.Context, context.CancelFunc, func()) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	shutdown := shutdownOnce(cleanup)
	go func() {
		select {
		case <-ctx.Done():
			shutdown()
			cancel()
		case <-runCtx.Done():
		}
	}()
	return runCtx, cancel, shutdown
}
