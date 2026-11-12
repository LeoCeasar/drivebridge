package ui

import (
	"context"
	"errors"
	"net/http"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

type automaticAuthBackend interface {
	AuthProgress(context.Context, string) (manager.AutomaticLoginView, error)
}

func publicAutomaticLogin(view manager.AutomaticLoginView) manager.AutomaticLoginView {
	switch view.State {
	case "waiting_authorization", "opening_login", "waiting_login", "ready", "failed":
	default:
		view.State = "failed"
	}
	switch view.Error {
	case "", "unauthorized", "identity_changed", "driver_unavailable", "pending", "starting", "config", "network", "operation_failed", "mount_archived", "expired", "canceled", "auth_reconcile_failed", "stale", "partial", "unsupported", "needs_runtime", "needs_browser", "needs_login", "browser_disconnected":
	default:
		view.Error = "operation_failed"
	}
	if view.Flow.URL != "" && !safeHTTPS(view.Flow.URL) {
		view.Flow.URL = ""
		view.State = "failed"
		view.Error = "operation_failed"
	}
	return view
}
func (h *Host) authProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w, "POST")
		return
	}
	var input struct {
		FlowID string `json:"flow_id"`
	}
	if decode(w, r, &input) != nil || !validID(input.FlowID) {
		respondError(w, 400, "invalid_request")
		return
	}
	backend, ok := h.backend.(automaticAuthBackend)
	if !ok {
		respondError(w, 503, "unavailable")
		return
	}
	view, err := backend.AuthProgress(r.Context(), input.FlowID)
	if err != nil {
		apiError(w, err)
		return
	}
	respond(w, 200, publicAutomaticLogin(view))
}
func (h *Host) respondAuthFlow(w http.ResponseWriter, flow auth.AuthFlow) {
	_, automatic := h.backend.(automaticAuthBackend)
	respond(w, 200, struct {
		auth.AuthFlow
		AutoComplete bool `json:"auto_complete"`
	}{flow, automatic})
}

// Authorization start errors with a specific, actionable meaning for the page.
// Shape (same as every other API error): {"code","retryable","message"}.
const (
	// authReconcileFailedCode: an interrupted earlier authorization (expired QR
	// code, restarted desktop) is recorded, and checking the saved application
	// configuration failed. Nothing was created or dropped. HTTP 503, retryable.
	authReconcileFailedCode = "auth_reconcile_failed"
	// authIdentityChangedCode: the authorizing Feishu user/app does not match the
	// identity already bound to this account. Nothing was changed. HTTP 409.
	authIdentityChangedCode = "auth_identity_changed"
)

const (
	authReconcileFailedMessage = "上次授权被中断，暂时无法核对已保存的应用配置和授权结果。已保存的应用配置、授权身份和待上传数据均保留，未创建新账号。请稍后再次点击授权；如仍失败，请重启 DriveBridge 后重试。"
	authIdentityChangedMessage = "本次授权的飞书账号与该账号盘已绑定的身份不一致，未做任何更改，待上传数据均保留。请使用原来的飞书账号重新授权。"
)

// authStartError reports a failed /api/auth/start. Busy states keep their
// generic local_busy answer; reconcile and identity failures get dedicated codes
// instead of the bare "offline" 503.
func authStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrPending), errors.Is(err, auth.ErrBusy), errors.Is(err, auth.ErrReferencesBusy):
		apiError(w, err)
	case errors.Is(err, model.ErrIdentityChanged):
		respond(w, http.StatusConflict, map[string]any{"code": authIdentityChangedCode, "retryable": false, "message": authIdentityChangedMessage})
	case errors.Is(err, auth.ErrReconcileFailed), errors.Is(err, auth.ErrNeedsReconcile):
		respond(w, http.StatusServiceUnavailable, map[string]any{"code": authReconcileFailedCode, "retryable": true, "message": authReconcileFailedMessage})
	default:
		apiError(w, err)
	}
}

// validAuthStartFlow accepts a flow with a safe HTTPS authorization link, or the
// link-less "reconciled" flow of an interrupted authorization that had already
// completed (the page tracks it through /api/auth/status like any other flow).
func validAuthStartFlow(flow auth.AuthFlow) bool {
	if flow.ID == "" {
		return false
	}
	return safeHTTPS(flow.URL) || (flow.Kind == "reconciled" && flow.URL == "")
}
