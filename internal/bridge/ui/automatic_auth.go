package ui

import (
	"context"
	"net/http"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/manager"
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
	case "", "unauthorized", "identity_changed", "driver_unavailable", "pending", "starting", "config", "network", "operation_failed", "mount_archived", "expired", "canceled", "stale", "partial", "unsupported", "needs_runtime", "needs_browser", "needs_login", "browser_disconnected":
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
