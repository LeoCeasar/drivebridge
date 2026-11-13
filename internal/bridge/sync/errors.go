package syncer

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

var errRemoteDrift = errors.New("remote_drift")

func classify(err error) string {
	var content *markdown.ContentError
	if errors.As(err, &content) {
		return "unsupported_content"
	}
	switch {
	case errors.Is(err, errRemoteDrift):
		return "remote_drift"
	case errors.Is(err, history.ErrGitUnavailable):
		return "history_git_unavailable"
	case errors.Is(err, history.ErrIntegrity) || errors.Is(err, history.ErrLimit):
		return "unsupported_content"
	case errors.Is(err, model.ErrIdentityChanged):
		return "identity_changed"
	case errors.Is(err, model.ErrUnknownResult):
		return "unknown_result"
	case errors.Is(err, model.ErrUnsupportedContent):
		return "unsupported_content"
	case errors.Is(err, store.ErrSpace) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT):
		return "insufficient_space"
	case errors.Is(err, syscall.EIO):
		return "io_error"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	var api *larkcli.Error
	if errors.As(err, &api) {
		c := strings.ToLower(api.Code + " " + api.Type + " " + api.Subtype)
		if strings.Contains(c, "permission") || strings.Contains(c, "scope") || strings.Contains(c, "auth") {
			return "permission_denied"
		}
		if strings.Contains(c, "rate") || api.RetryAfter > 0 {
			return "rate_limited"
		}
	}
	return "network_or_protocol"
}
func (e *Engine) pause(ctx context.Context, j model.Job, err error) error {
	e.deferRetry(j.ID, err)
	code := classify(err)
	state := model.StatusPaused
	if code == "unsupported_content" || code == "permission_denied" || code == "identity_changed" || code == "history_git_unavailable" {
		state = model.StatusBlocked
	}
	diagnostic := &model.SyncDiagnostic{Kind: code, Message: diagnosticMessage(code)}
	if code == "unsupported_content" {
		// Capture source detail once, from the immutable job input. Status reads only
		// the journal; it never fetches or reparses remote content.
		var body []byte
		if r, readErr := e.store.ReadBlob(context.WithoutCancel(ctx), j.Local); readErr == nil {
			captured, readErr := io.ReadAll(io.LimitReader(r, markdown.MaxMarkdownBytes+1))
			closeErr := r.Close()
			if readErr == nil && closeErr == nil && len(captured) <= markdown.MaxMarkdownBytes {
				body = captured
			}
		}
		if detail := markdown.DiagnoseContent(body, err); detail != nil {
			diagnostic = detail
		}
	}
	saveErr := e.store.PauseJobWithDiagnostic(context.WithoutCancel(ctx), j.ID, state, code, diagnostic)
	return errors.Join(err, saveErr)
}
func (e *Engine) conflict(ctx context.Context, j model.Job, remote model.Snapshot, reason string) error {
	current, err := e.store.Entry(ctx, j.EntryID)
	if err != nil {
		return err
	}
	// The captured generation may be older than a save accepted while we fetched.
	// Freeze the newest visible input for the user's conflict resolution instead.
	if current.Generation != j.Generation {
		if err = e.store.PauseJob(ctx, j.ID, model.StatusPaused, "newer_generation_conflict"); err != nil {
			return err
		}
	}
	err = e.store.SaveConflict(ctx, model.Conflict{EntryID: current.ID, Generation: current.Generation, Base: j.BaseLocal, Local: current.Local, Remote: remote.ContentBlob(), RemoteRevision: remote.ConflictVersion(), Draft: current.Local, Reason: reason})
	return errors.Join(model.ErrVersionChanged, err)
}

// Transport errors may contain CLI arguments or credentials. Persist a safe
// classified explanation instead of serializing their raw Error strings.
func diagnosticMessage(code string) string {
	switch code {
	case "unsupported_content":
		return "内容无法安全转换或写回，请检查文件结构。"
	case "permission_denied":
		return "当前账号或本机目录权限不足，无法完成同步。"
	case "identity_changed":
		return "账号身份发生变化，需要重新核验。"
	case "history_git_unavailable":
		return "自动合并组件暂不可用。"
	case "insufficient_space":
		return "本机存储空间不足，无法完成同步。"
	case "io_error":
		return "本机读写失败，无法完成同步。"
	case "remote_drift":
		return "云端版本持续变化，后台将稍后重试。"
	case "unknown_result":
		return "请求结果尚未确认，需等待云端回读核验。"
	case "rate_limited":
		return "云端暂时限制请求频率，后台将稍后重试。"
	case "request_timeout":
		return "云端请求等待超时，修改已保留，后台将稍后重试。"
	case "canceled":
		return "同步请求已取消，修改保留在本机。"
	default:
		return "网络连接或服务响应未通过核验，后台将重试。"
	}
}
