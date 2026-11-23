package syncer

import (
	"context"
	"errors"
	"io"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
	"github.com/rclone/rclone/internal/bridge/store"
)

var errRemoteDrift = errors.New("remote_drift")
var errParentPending = errors.New("parent folder is awaiting cloud creation")

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
	case errors.Is(err, model.ErrUnknownResult):
		return "unknown_result"
	case errors.Is(err, errParentPending):
		return "parent_pending"
	case errors.Is(err, store.ErrSpace):
		return "insufficient_space"
	}
	// Compatibility codes remain for the old state-machine/API contract. The
	// durable Online record carries the precise shared cause independently.
	switch cause := online.Cause(err); cause {
	case "verification_unavailable", "connection_error", "transport_error", "dns_error":
		return "network_or_protocol"
	case "scope_missing":
		return "permission_denied"
	default:
		return cause
	}
}
func (e *Engine) pause(ctx context.Context, j model.Job, err error) error {
	e.deferRetry(ctx, j.ID, err)
	code := pauseCode(ctx, err)
	state := model.StatusPaused
	if code == "unsupported_content" || code == "permission_denied" || code == "auth_required" || code == "credential_unavailable" || code == "identity_changed" || code == "history_git_unavailable" || code == "unclassified_error" || code == "protocol_error" || code == "api_error_unknown" || code == "tls_error" || code == "quota_exceeded" || code == "canceled" {
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
	phase := "unknown"
	if code == "unsupported_content" {
		phase = "conversion"
	}
	return online.AtPhaseIfUnset(errors.Join(err, saveErr), "update", phase)
}

func pauseCode(ctx context.Context, err error) string {
	code := classify(err)
	if code == "canceled" {
		switch online.CancellationOrigin(ctx, err) {
		case "runtime_shutdown":
			return "runtime_interrupted"
		case "caller_gone":
			return "caller_interrupted"
		case "phase_deadline", "parent_deadline":
			return "request_timeout"
		}
	}
	return code
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
	case "parent_pending":
		return "上级文件夹尚未完成云端创建，完成后将自动继续同步。"
	case "unsupported_content":
		return "内容无法安全转换或写回，请检查文件结构。"
	case "permission_denied":
		return "当前账号或本机目录权限不足，无法完成同步。"
	case "auth_required":
		return "用户授权已失效，需要使用原账号重新授权。"
	case "credential_unavailable":
		return "本机凭据存储暂不可访问，请检查钥匙串或凭据管理器。"
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
	case "runtime_interrupted":
		return "应用退出或运行环境中断，保留原操作以便恢复后安全核验。"
	case "caller_interrupted":
		return "交互请求已结束，已保存的任务保留在后台。"
	default:
		return online.Message(code)
	}
}
