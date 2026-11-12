package ui

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type HistoryBackend interface {
	HistoryList(context.Context, manager.HistoryQuery) (manager.HistoryPage, error)
	HistoryDetail(context.Context, model.ProfileID, string) (manager.HistoryDetail, error)
	HistoryBody(context.Context, model.ProfileID, string, string) ([]byte, error)
	HistoryDiff(context.Context, model.ProfileID, string, string) (history.Difference, error)
	HistoryRestore(context.Context, model.HistoryRestoreRequest) (model.HistoryRestoreResult, error)
	HistoryStates(context.Context) (map[model.MountID]model.HistoryStatus, error)
}

func (b *ManagerBackend) HistoryList(ctx context.Context, q manager.HistoryQuery) (manager.HistoryPage, error) {
	return b.manager.HistoryList(ctx, q)
}
func (b *ManagerBackend) HistoryDetail(ctx context.Context, p model.ProfileID, id string) (manager.HistoryDetail, error) {
	return b.manager.HistoryDetail(ctx, p, id)
}
func (b *ManagerBackend) HistoryBody(ctx context.Context, p model.ProfileID, id, role string) ([]byte, error) {
	return b.manager.HistoryBody(ctx, p, id, role)
}
func (b *ManagerBackend) HistoryDiff(ctx context.Context, p model.ProfileID, left, right string) (history.Difference, error) {
	return b.manager.HistoryDiff(ctx, p, left, right)
}
func (b *ManagerBackend) HistoryRestore(ctx context.Context, r model.HistoryRestoreRequest) (model.HistoryRestoreResult, error) {
	return b.manager.HistoryRestore(ctx, r)
}
func (b *ManagerBackend) HistoryStates(ctx context.Context) (map[model.MountID]model.HistoryStatus, error) {
	return b.manager.HistoryStates(ctx)
}
func historyAPIError(w http.ResponseWriter, err error) {
	status, code, message := 503, "history_unavailable", "历史服务暂时不可用。本地文件仍保留，请检查历史组件状态后重试。"
	switch {
	case errors.Is(err, history.ErrBodyUnavailable):
		status, code, message = 404, "body_not_cached", "此版本的正文未取得或已不在保留范围内，不能作为空文件读取。"
	case errors.Is(err, history.ErrLimit):
		status, code, message = 413, "history_limit", "正文超出页面读取上限，请使用 CLI 或显式导出。"
	case errors.Is(err, model.ErrGenerationChanged) || errors.Is(err, store.ErrDestinationChanged) || errors.Is(err, store.ErrFrozen) || errors.Is(err, store.ErrBusy) || errors.Is(err, manager.ErrPending):
		status, code, message = 409, "history_state_changed", "文件、目标位置或打开状态已变化。请关闭编辑器并刷新后重新核对；当前内容未覆盖。"
	case errors.Is(err, model.ErrIdentityChanged):
		status, code, message = 403, "history_identity_changed", "账号或文件身份与历史记录不一致。"
	case errors.Is(err, manager.ErrArchived):
		status, code, message = 409, "history_mount_archived", "此挂载已移除。历史仍可查看，恢复前请先启用挂载。"
	case errors.Is(err, manager.ErrConfig):
		status, code, message = 400, "invalid_request", "请检查历史版本及目标参数。"
	}
	respond(w, status, map[string]string{"code": code, "message": message})
}
func (h *Host) historyRoutes(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/history/pin" || r.URL.Path == "/api/history/policy" {
		h.historyMaintenance(w, r)
		return
	}
	backend, ok := h.backend.(HistoryBackend)
	if !ok {
		respondError(w, 503, "history_unavailable")
		return
	}
	if len(r.URL.RawQuery) > 8192 {
		respondError(w, 400, "invalid_request")
		return
	}
	p := model.ProfileID(r.URL.Query().Get("profile_id"))
	if p != "" && !validID(string(p)) {
		respondError(w, 400, "invalid_request")
		return
	}
	if r.URL.Path == "/api/history/restore" {
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var req model.HistoryRestoreRequest
		if decode(w, r, &req) != nil || !validID(string(req.ProfileID)) || !validID(string(req.MountID)) || !validID(string(req.EntryID)) || !validID(string(req.ParentID)) || !validID(req.RequestID) {
			respondError(w, 400, "invalid_request")
			return
		}
		result, err := backend.HistoryRestore(r.Context(), req)
		if err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, result)
		return
	}
	if r.Method != "GET" {
		method(w, "GET")
		return
	}
	switch {
	case r.URL.Path == "/api/history" || r.URL.Path == "/api/history/pending":
		q := manager.HistoryQuery{ProfileID: p, Cursor: r.URL.Query().Get("cursor"), Query: history.Query{EntryID: model.EntryID(r.URL.Query().Get("entry_id")), MountID: model.MountID(r.URL.Query().Get("mount_id")), Kind: r.URL.Query().Get("kind"), Search: r.URL.Query().Get("search")}}
		if q.EntryID != "" && !validID(string(q.EntryID)) || q.MountID != "" && !validID(string(q.MountID)) {
			respondError(w, 400, "invalid_request")
			return
		}
		if value := r.URL.Query().Get("limit"); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 200 {
				respondError(w, 400, "invalid_request")
				return
			}
			q.Limit = n
		}
		if r.URL.Path == "/api/history/pending" {
			if local, ok := h.backend.(HistoryPendingBackend); ok {
				result, err := local.HistoryPending(r.Context(), q)
				if err != nil {
					historyAPIError(w, err)
					return
				}
				respond(w, 200, result)
			} else {
				respond(w, 200, manager.HistoryPage{Versions: []history.Version{}})
			}
			return
		}
		result, err := backend.HistoryList(r.Context(), q)
		if err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, result)
	case r.URL.Path == "/api/history/status":
		result, err := backend.HistoryStates(r.Context())
		if err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, result)
	case r.URL.Path == "/api/history/diff":
		if p == "" {
			respondError(w, 400, "invalid_request")
			return
		}
		result, err := backend.HistoryDiff(r.Context(), p, r.URL.Query().Get("left"), r.URL.Query().Get("right"))
		if err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, result)
	case r.URL.Path == "/api/history/body":
		if p == "" {
			respondError(w, 400, "invalid_request")
			return
		}
		role := r.URL.Query().Get("role")
		body, err := backend.HistoryBody(r.Context(), p, r.URL.Query().Get("version_id"), role)
		if err != nil {
			historyAPIError(w, err)
			return
		}
		if strings.HasPrefix(role, "media:") {
			kind := http.DetectContentType(body)
			switch kind {
			case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif":
				w.Header().Set("Content-Type", kind)
				w.Write(body)
			default:
				respondError(w, 415, "unsupported_content")
			}
			return
		}
		text := utf8.Valid(body) && bytes.IndexByte(body, 0) < 0
		content := ""
		if text {
			content = string(body)
		}
		respond(w, 200, map[string]any{"text": text, "content": content, "size": len(body)})
	case strings.HasPrefix(r.URL.Path, "/api/history/versions/"):
		if p == "" {
			respondError(w, 400, "invalid_request")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/history/versions/")
		result, err := backend.HistoryDetail(r.Context(), p, id)
		if err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, result)
	default:
		respondError(w, 404, "not_found")
	}
}

type HistoryMaintenanceBackend interface {
	HistoryPin(context.Context, model.ProfileID, string, bool) error
	HistoryPolicy(context.Context, model.ProfileID) (model.HistoryPolicy, error)
	SetHistoryPolicy(context.Context, model.ProfileID, model.HistoryPolicy) error
}

func (b *ManagerBackend) HistoryPin(ctx context.Context, p model.ProfileID, id string, pin bool) error {
	return b.manager.HistoryPin(ctx, p, id, pin)
}
func (b *ManagerBackend) HistoryPolicy(ctx context.Context, p model.ProfileID) (model.HistoryPolicy, error) {
	return b.manager.HistoryPolicy(ctx, p)
}
func (b *ManagerBackend) SetHistoryPolicy(ctx context.Context, p model.ProfileID, policy model.HistoryPolicy) error {
	return b.manager.SetHistoryPolicy(ctx, p, policy)
}
func (h *Host) historyMaintenance(w http.ResponseWriter, r *http.Request) {
	b, ok := h.backend.(HistoryMaintenanceBackend)
	if !ok {
		respondError(w, 503, "unavailable")
		return
	}
	if r.URL.Path == "/api/history/pin" {
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var in struct {
			ProfileID model.ProfileID `json:"profile_id"`
			VersionID string          `json:"version_id"`
			Pinned    bool            `json:"pinned"`
		}
		if decode(w, r, &in) != nil || !validID(string(in.ProfileID)) {
			respondError(w, 400, "invalid_request")
			return
		}
		if err := b.HistoryPin(r.Context(), in.ProfileID, in.VersionID, in.Pinned); err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"pinned": in.Pinned})
		return
	}
	if r.Method == "GET" {
		p := model.ProfileID(r.URL.Query().Get("profile_id"))
		if !validID(string(p)) {
			respondError(w, 400, "invalid_request")
			return
		}
		policy, err := b.HistoryPolicy(r.Context(), p)
		if err != nil {
			historyAPIError(w, err)
			return
		}
		respond(w, 200, policy)
		return
	}
	if r.Method != "POST" {
		method(w, "GET, POST")
		return
	}
	var in struct {
		ProfileID model.ProfileID     `json:"profile_id"`
		Policy    model.HistoryPolicy `json:"policy"`
	}
	if decode(w, r, &in) != nil || !validID(string(in.ProfileID)) || !in.Policy.Valid() {
		respondError(w, 400, "invalid_request")
		return
	}
	if err := b.SetHistoryPolicy(r.Context(), in.ProfileID, in.Policy); err != nil {
		historyAPIError(w, err)
		return
	}
	respond(w, 200, in.Policy)
}

type HistoryPendingBackend interface {
	HistoryPending(context.Context, manager.HistoryQuery) (manager.HistoryPage, error)
}

func (b *ManagerBackend) HistoryPending(ctx context.Context, q manager.HistoryQuery) (manager.HistoryPage, error) {
	return b.manager.HistoryPending(ctx, q)
}
