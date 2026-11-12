package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

//go:embed assets/*
var assets embed.FS

const maxBody = 15 << 20

func ioLimit(r io.Reader, n int64) io.Reader { return io.LimitReader(r, n) }
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		return errors.New("JSON required")
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func respondError(w http.ResponseWriter, status int, code string) {
	messages := map[string]string{"unauthorized": "请重新从本机打开管理界面。", "forbidden": "此请求未通过本机会话校验。", "invalid_request": "请检查输入内容。", "not_found": "该项目已不存在，请刷新。", "version_changed": "原版、本地或云端内容已变化，草稿已保留。", "offline": "暂时无法复核云端内容，草稿已保留。", "unavailable": "服务暂时不可用，请稍后重试。", "method_not_allowed": "不支持此操作。", "unsupported_content": "此文件不能在文本合并页提交。"}
	messages["status_unavailable"] = "本机状态读取暂未完成，请稍后刷新。"
	messages["mount_not_running"] = "挂载尚未运行，请先启动挂载。"
	messages["folder_unavailable"] = "无法找到挂载目录，请确认挂载正在运行。"
	messages["no_desktop"] = "当前环境无法打开文件管理器，请手动打开挂载位置。"
	messages["unsupported_content"] = "此内容暂不支持安全提交，草稿已保留。请检查文件格式或文档结构。"
	messages["mount_archived"] = "此挂载已移除，请先恢复挂载配置，再启动同步。"
	messages["mount_in_use"] = "暂时不能移除：请先关闭正在使用该挂载的文件，等待本机写入完成后重试；已有数据会保留。"
	messages["local_busy"] = "本机正在处理其他操作，请稍后重试。"
	messages["user_authorization_required"] = "应用已配置，请再次点击授权以完成用户登录。"
	messages["refresh_required"] = "云端尚未重新核验，草稿已保留。请恢复连接后刷新差异。"
	messages["needs_runtime"] = "请先安装 Microsoft Edge WebView2 Runtime，再重新打开应用登录窗口。已有目录、文件和授权会保留。"
	messages["discovery_unclassified"] = "暂时无法核验目录属于个人还是共享；旧清单已保留，请重新扫描后重试。"
	messages["discovery_unavailable"] = "目录操作暂未完成，已有清单仍保留。请查看账号状态，必要时重新打开应用登录窗口或检查目录访问权限。"
	respond(w, status, map[string]any{"code": code, "retryable": status == 503, "message": messages[code]})
}
func apiError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrPending), errors.Is(err, store.ErrLocked), errors.Is(err, store.ErrBusy), errors.Is(err, auth.ErrBusy), errors.Is(err, auth.ErrReferencesBusy):
		respondError(w, 503, "local_busy")
	case errors.Is(err, manager.ErrArchived):
		respondError(w, 409, "mount_archived")
	case errors.Is(err, ErrMountNotRunning):
		respondError(w, 409, "mount_not_running")
	case errors.Is(err, platform.ErrInvalidFolder):
		respondError(w, 409, "folder_unavailable")
	case errors.Is(err, platform.ErrNoDesktop):
		respondError(w, 503, "no_desktop")
	case errors.Is(err, manager.ErrConfig):
		respondError(w, 400, "invalid_request")
	case errors.Is(err, auth.ErrUserMissing):
		respondError(w, http.StatusConflict, "user_authorization_required")
	case errors.Is(err, model.ErrVersionChanged) || errors.Is(err, model.ErrGenerationChanged) || errors.Is(err, model.ErrIdentityChanged):
		respondError(w, 409, "version_changed")
	case errors.Is(err, os.ErrNotExist):
		respondError(w, 404, "not_found")
	case errors.Is(err, model.ErrUnsupportedContent):
		respondError(w, 422, "unsupported_content")
	default:
		respondError(w, 503, "offline")
	}
}
func headers(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
}
func (h *Host) serve(w http.ResponseWriter, r *http.Request) {
	headers(w)
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() || r.Host != h.listener.Addr().String() {
		respondError(w, 403, "forbidden")
		return
	}
	p := r.URL.Path
	if p == "" || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00") || strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(p, "//") {
		respondError(w, 400, "invalid_request")
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != h.base {
		respondError(w, 403, "forbidden")
		return
	}
	if p == "/internal/open" {
		h.openControl(w, r)
		return
	}
	if r.Method == http.MethodGet && (p == "/" || p == "/app.js" || p == "/auth-view.js" || p == "/diff-view.js" || p == "/recovery-view.js" || p == "/batch-view.js" || p == "/discovery-view.js" || p == "/style.css") {
		name := "assets" + p
		if p == "/" {
			name = "assets/index.html"
		}
		b, err := assets.ReadFile(name)
		if err != nil {
			respondError(w, 404, "not_found")
			return
		}
		switch p {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case "/app.js", "/auth-view.js", "/diff-view.js", "/recovery-view.js", "/batch-view.js", "/discovery-view.js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		default:
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		_, _ = w.Write(b)
		return
	}
	if p == "/api/session" && r.Method == http.MethodPost {
		if origin != h.base {
			respondError(w, 403, "forbidden")
			return
		}
		h.newSession(w, r)
		return
	}
	session, ok := h.authorize(r)
	if !ok {
		respondError(w, 403, "unauthorized")
		return
	}
	if r.Method != http.MethodGet && (origin != h.base || !same(r.Header.Get("X-CSRF-Token"), session.csrf)) {
		respondError(w, 403, "forbidden")
		return
	}
	switch p {
	case "/api/session":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		respond(w, 200, map[string]string{"csrf": session.csrf})
		return
	case "/api/status":
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		v, err := h.backend.Status(r.Context())
		if err != nil {
			respondError(w, 503, "status_unavailable")
			return
		}
		for i := range v.Mounts {
			v.Mounts[i].State.LastError = safeCode(v.Mounts[i].State.LastError)
			v.Mounts[i].StatusWarning = safeCode(v.Mounts[i].StatusWarning)
		}
		_, v.CanRetry = h.backend.(SyncRetrier)
		v.Platform, v.OpenFolderLabel = hostGOOS, FolderOpenLabel(hostGOOS)
		_, v.CanOpenFolder = h.backend.(FolderOpener)
		respond(w, 200, v)
		return
	case "/api/config":
		switch r.Method {
		case http.MethodGet:
			v, err := h.backend.Config(r.Context())
			if err != nil {
				apiError(w, err)
				return
			}
			v.OpenFolderLabel = FolderOpenLabel(hostGOOS)
			_, v.CanOpenFolder = h.backend.(FolderOpener)
			respond(w, 200, v)
		case http.MethodPost:
			var in ConfigRequest
			if decode(w, r, &in) != nil || (in.Action != "profile" && in.Action != "remote" && in.Action != "mount" && in.Action != "mount_policy") {
				respondError(w, 400, "invalid_request")
				return
			}
			if in.Action == "mount_policy" && (!validID(string(in.MountID)) || in.BackgroundPullExcludedExtensions == nil) {
				respondError(w, 400, "invalid_request")
				return
			}
			v, err := h.backend.Configure(r.Context(), in)
			if err != nil {
				apiError(w, err)
				return
			}
			respond(w, 200, v)
		default:
			method(w, "GET, POST")
		}
		return
	case "/api/auth/start":
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var in struct {
			ProfileID model.ProfileID `json:"profile_id"`
			Recovery  bool            `json:"recovery,omitempty"`
		}
		if decode(w, r, &in) != nil || !validID(string(in.ProfileID)) {
			respondError(w, 400, "invalid_request")
			return
		}
		var flow auth.AuthFlow
		var err error
		if in.Recovery {
			if recovery, ok := h.backend.(interface {
				AuthStartRecovery(context.Context, model.ProfileID) (auth.AuthFlow, error)
			}); ok {
				flow, err = recovery.AuthStartRecovery(r.Context(), in.ProfileID)
			} else {
				respondError(w, 503, "unavailable")
				return
			}
		} else {
			flow, err = h.backend.AuthStart(r.Context(), in.ProfileID)
		}
		if err != nil {
			authStartError(w, err)
			return
		}
		if !validAuthStartFlow(flow) {
			respondError(w, 503, "unavailable")
			return
		}
		h.respondAuthFlow(w, flow)
		return
	case "/api/auth/status":
		h.authProgress(w, r)
		return
	case "/api/auth/complete":
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var in struct {
			FlowID string `json:"flow_id"`
		}
		if decode(w, r, &in) != nil || !validID(in.FlowID) {
			respondError(w, 400, "invalid_request")
			return
		}
		if err := h.backend.AuthComplete(r.Context(), in.FlowID); err != nil {
			apiError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"authorized": true})
		return
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) >= 2 && parts[0] == "api" && parts[1] == "discovery" {
		h.discovery(w, r, parts[2:])
		return
	}
	if len(parts) >= 3 && parts[0] == "api" && parts[1] == "conflicts" && validID(parts[2]) {
		h.conflict(w, r, parts[2], parts[3:])
		return
	}
	if len(parts) == 4 && parts[0] == "api" && parts[1] == "mounts" && validID(parts[2]) && (parts[3] == "start" || parts[3] == "stop" || parts[3] == "retry" || parts[3] == "archive" || parts[3] == "restore" || parts[3] == "open") {
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var empty struct{}
		if decode(w, r, &empty) != nil {
			respondError(w, 400, "invalid_request")
			return
		}
		var err error
		if parts[3] == "retry" {
			retrier, ok := h.backend.(SyncRetrier)
			if !ok {
				respondError(w, 503, "unavailable")
				return
			}
			err = retrier.Retry(r.Context(), model.MountID(parts[2]))
		} else if parts[3] == "open" {
			opener, ok := h.backend.(FolderOpener)
			if !ok {
				respondError(w, 503, "unavailable")
				return
			}
			err = opener.OpenMountFolder(r.Context(), model.MountID(parts[2]))
		} else {
			err = h.backend.Mount(r.Context(), model.MountID(parts[2]), parts[3])
		}
		if err != nil {
			if parts[3] == "archive" && errors.Is(err, manager.ErrPending) {
				respondError(w, 409, "mount_in_use")
			} else {
				apiError(w, err)
			}
			return
		}
		respond(w, 200, map[string]bool{"accepted": true})
		return
	}
	respondError(w, 404, "not_found")
}
func method(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	respondError(w, 405, "method_not_allowed")
}
func safeCode(s string) string {
	switch s {
	case "", "offline", "auth_required", "permission_denied", "version_changed", "identity_changed", "worker_unavailable", "blocked", "pending", "unsupported_content":
		return s
	}
	return "operation_failed"
}

func safeHTTPS(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && !strings.ContainsAny(raw, "\r\n\x00")
}
func (h *Host) openControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		method(w, "POST")
		return
	}
	if r.Header.Get("Origin") != "" || !same(r.Header.Get("Authorization"), "Bearer "+h.control) {
		respondError(w, 403, "forbidden")
		return
	}
	var in struct {
		Instance   string           `json:"instance"`
		ConflictID model.ConflictID `json:"conflict_id"`
	}
	if decode(w, r, &in) != nil || in.Instance != h.instance {
		respondError(w, 403, "forbidden")
		return
	}
	u, err := h.BootstrapURL(in.ConflictID)
	if err != nil {
		respondError(w, 400, "invalid_request")
		return
	}
	respond(w, 200, map[string]string{"Instance": h.instance, "URL": u})
}
func (h *Host) conflict(w http.ResponseWriter, r *http.Request, id string, tail []string) {
	cid := model.ConflictID(id)
	if len(tail) == 0 {
		if r.Method != "GET" {
			method(w, "GET")
			return
		}
		in, err := h.backend.Conflict(r.Context(), cid)
		if err != nil {
			apiError(w, err)
			return
		}
		if in.Conflict.EntryID != in.Entry.ID || in.Conflict.ID != cid {
			respondError(w, 409, "version_changed")
			return
		}
		v := ConflictView{Hunks: []DiffHunk{}, Stale: in.Stale, RefreshWarning: safeCode(in.RefreshWarning), HasDraft: in.Conflict.Draft != "" || in.Draft != nil, ID: cid, EntryID: in.Entry.ID, Generation: in.Conflict.Generation, RemoteRevision: in.Conflict.RemoteRevision, Name: in.Entry.Remote, Text: in.Text}
		v.Reason = in.Conflict.Reason
		v.RemoteDeleted = in.Conflict.Reason == model.ConflictRemoteDeleted
		v.LocalDeletePending = in.Conflict.Reason == model.ConflictLocalDeletePending
		_, v.CanExportCopies = h.backend.(ConflictExporter)
		if in.Text && utf8.Valid(in.Base) && utf8.Valid(in.Local) && utf8.Valid(in.Remote) && utf8.Valid(in.Draft) {
			v.Base = string(in.Base)
			v.Local = string(in.Local)
			v.Remote = string(in.Remote)
			v.Draft = string(in.Draft)
			v.Hunks = diffHunks(v.Base, v.Local, v.Remote)
			if v.Hunks == nil {
				v.Hunks = []DiffHunk{}
			}
		} else {
			v.Text = false
		}
		if safeHTTPS(in.SourceURL) {
			v.SourceURL = in.SourceURL
		}
		respond(w, 200, v)
		return
	}
	if len(tail) != 1 {
		respondError(w, 404, "not_found")
		return
	}
	switch tail[0] {
	case "copies":
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var empty struct{}
		if decode(w, r, &empty) != nil {
			respondError(w, 400, "invalid_request")
			return
		}
		exporter, ok := h.backend.(ConflictExporter)
		if !ok {
			respondError(w, 503, "unavailable")
			return
		}
		copies, err := exporter.ExportConflict(r.Context(), cid)
		if err != nil {
			apiError(w, err)
			return
		}
		respond(w, 200, copies)
	case "draft":
		if r.Method != "PUT" {
			method(w, "PUT")
			return
		}
		var in struct {
			Content []byte `json:"content"`
		}
		if decode(w, r, &in) != nil || len(in.Content) > 10<<20 {
			respondError(w, 400, "invalid_request")
			return
		}
		if err := h.backend.SaveDraft(r.Context(), cid, in.Content); err != nil {
			apiError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"saved": true})
	case "resolve":
		if r.Method != "POST" {
			method(w, "POST")
			return
		}
		var in model.ResolveRequest
		if decode(w, r, &in) != nil || in.ConflictID != cid || len(in.Content) > 10<<20 {
			respondError(w, 400, "invalid_request")
			return
		}
		timing := newServerTiming()
		// Version checks read the stored conflict only; the single cloud recheck
		// happens inside Resolve, so a batch does not pay for extra cloud reads.
		readVersions := h.backend.Conflict
		if reader, ok := h.backend.(ResolveVersionReader); ok {
			readVersions = reader.ConflictForResolve
		}
		current, err := readVersions(r.Context(), cid)
		if err != nil {
			apiError(w, err)
			return
		}
		reason := current.Conflict.Reason
		deleted := reason == model.ConflictRemoteDeleted || reason == model.ConflictLocalDeletePending
		if (deleted && in.Action != model.ResolveRestoreLocal && in.Action != model.ResolveAcceptDelete) || (!deleted && in.Action != "") {
			respondError(w, 400, "invalid_request")
			return
		}
		// A deletion decision may retain a binary blob without supplying content.
		// Following deletion must never replace a meaningful saved draft with nil.
		save := !deleted || (in.Action == model.ResolveRestoreLocal && in.Content != nil)
		if deleted && save && (!current.Text || !utf8.Valid(in.Content)) {
			respondError(w, 422, "unsupported_content")
			return
		}
		if save {
			if err := h.backend.SaveDraft(r.Context(), cid, in.Content); err != nil {
				apiError(w, err)
				return
			}
			timing.mark("draft")
			current, err = readVersions(r.Context(), cid)
			if err != nil {
				apiError(w, err)
				return
			}
			if current.Conflict.Reason != reason {
				respondError(w, 409, "version_changed")
				return
			}
		}
		timing.mark("check")
		if current.Conflict.EntryID != current.Entry.ID || current.Conflict.Generation != in.ExpectedGeneration || current.Entry.Generation != in.ExpectedGeneration || current.Conflict.RemoteRevision != in.ExpectedRemoteRevision {
			respondError(w, 409, "version_changed")
			return
		}
		if current.Stale {
			respondError(w, 503, "refresh_required")
			return
		}
		if (!deleted || save) && (!current.Text || !utf8.Valid(in.Content)) {
			respondError(w, 422, "unsupported_content")
			return
		}
		err = h.backend.Resolve(r.Context(), in)
		timing.mark("accept")
		timing.write(w)
		if err != nil {
			apiError(w, err)
			return
		}
		respond(w, 200, map[string]bool{"accepted": true})
	default:
		respondError(w, 404, "not_found")
	}
}

// PollConflicts discovers already-persisted conflicts; failure never removes them.
func (h *Host) PollConflicts(ctx context.Context) error {
	status, err := h.backend.Status(ctx)
	if err != nil {
		return err
	}
	for _, c := range status.Conflicts {
		if err = h.NotifyConflict(ctx, c.ID); err != nil {
			return err
		}
	}
	return nil
}
