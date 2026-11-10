package ui

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"path/filepath"
)

type desktopNotifier struct{}

func (desktopNotifier) Available() bool { return platform.DesktopAvailable() }
func (desktopNotifier) Notify(ctx context.Context, t, m string) error {
	return platform.Notify(ctx, t, m)
}
func (desktopNotifier) Open(ctx context.Context, u string) error { return platform.OpenBrowser(ctx, u) }
func (h *Host) NotifyConflict(ctx context.Context, id model.ConflictID) error {
	input, err := h.backend.Conflict(ctx, id)
	if err != nil {
		return err
	}
	h.mu.Lock()
	n := h.notices[id]
	if n.Revision != input.Conflict.RemoteRevision || n.Count == 0 {
		n.Count++
		n.Revision = input.Conflict.RemoteRevision
	}
	shouldOpen := !n.Opened && h.notifier.Available()
	if shouldOpen {
		n.Opened = true
	}
	h.notices[id] = n
	err = atomicJSON(filepath.Join(h.runtimeDir, "notifications.json"), h.notices)
	h.mu.Unlock()
	if err != nil {
		return err
	}
	if !shouldOpen {
		return nil
	}
	if claims, ok := h.backend.(interface {
		ClaimConflictNotification(context.Context, model.ConflictID) (bool, error)
	}); ok {
		claimed, claimErr := claims.ClaimConflictNotification(ctx, id)
		if claimErr != nil {
			return claimErr
		}
		if !claimed {
			return nil
		}
	}
	u, err := h.BootstrapURL(id)
	if err != nil {
		return err
	}
	notifyErr := h.notifier.Notify(ctx, "DriveBridge 需要合并文件", "本机草稿已保留，请查看合并页。")
	openErr := h.notifier.Open(ctx, u)
	if notifyErr != nil {
		return notifyErr
	}
	return openErr
}
