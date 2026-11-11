package ui

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

// DiscoveryBackend is optional so offline and fixture hosts keep their existing capabilities.
type DiscoveryBackend interface {
	DiscoveryViews(context.Context) ([]manager.DiscoveryView, error)
	StartDiscoveryLogin(context.Context, model.ProfileID, string) (manager.DiscoveryView, error)
	CompleteDiscoveryLogin(context.Context, model.ProfileID) (manager.DiscoveryView, error)
	RescanDiscovery(context.Context, model.ProfileID) (manager.DiscoveryView, error)
	SearchDiscoveryFolders(context.Context, model.ProfileID, string) ([]manager.DiscoveryCandidate, error)
	AddDiscoveryFolder(context.Context, model.ProfileID, string) (manager.DiscoveryView, error)
}

func discoveryCode(s string) string {
	switch s {
	case "", "complete", "partial", "stale", "needs_login", "needs_browser", "needs_extension", "browser_disconnected", "unsupported", "idle", "login_pending", "scanning", "unconfigured", "disabled":
		return s
	default:
		return "unavailable"
	}
}

// Never return upstream error text or the identity ledger to a browser.
func publicDiscovery(v manager.DiscoveryView) manager.DiscoveryView {
	v.Error = discoveryCode(v.Error)
	v.State = discoveryCode(v.State)
	v.Snapshot.Binding = discovery.Binding{}
	sources := make(map[string]discovery.SourceState, len(v.Snapshot.Sources))
	for k, source := range v.Snapshot.Sources {
		if k != discovery.Personal && k != discovery.SharedVisible && k != discovery.SharedHidden && k != discovery.Manual {
			continue
		}
		source.Status = discoveryCode(source.Status)
		source.Error = discoveryCode(source.Error)
		sources[k] = source
	}
	v.Snapshot.Sources = sources
	return v
}

func discoveryText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (h *Host) discovery(w http.ResponseWriter, r *http.Request, tail []string) {
	backend, ok := h.backend.(DiscoveryBackend)
	if !ok {
		respondError(w, 503, "unavailable")
		return
	}
	if len(tail) == 0 {
		if r.Method != http.MethodGet {
			method(w, "GET")
			return
		}
		views, err := backend.DiscoveryViews(r.Context())
		if err != nil {
			respondError(w, 503, "discovery_unavailable")
			return
		}
		out := make([]manager.DiscoveryView, 0, len(views))
		for _, v := range views {
			out = append(out, publicDiscovery(v))
		}
		respond(w, 200, out)
		return
	}
	if len(tail) != 2 || !validID(tail[0]) {
		respondError(w, 400, "invalid_request")
		return
	}
	action := tail[1]
	if action != "login" && action != "complete" && action != "rescan" && action != "search" && action != "add" {
		respondError(w, 400, "invalid_request")
		return
	}
	if r.Method != http.MethodPost {
		method(w, "POST")
		return
	}
	var in struct {
		Origin string `json:"origin,omitempty"`
		Input  string `json:"input,omitempty"`
	}
	if decode(w, r, &in) != nil || !discoveryText(in.Origin, 2048) || !discoveryText(in.Input, 4096) || (action != "login" && in.Origin != "") || (action != "search" && action != "add" && in.Input != "") || ((action == "search" || action == "add") && strings.TrimSpace(in.Input) == "") {
		respondError(w, 400, "invalid_request")
		return
	}
	if action == "search" && utf8.RuneCountInString(strings.TrimSpace(in.Input)) > 30 {
		respondError(w, 400, "invalid_request")
		return
	}
	if in.Origin != "" {
		u, err := url.Parse(in.Origin)
		if err != nil || !safeHTTPS(in.Origin) || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			respondError(w, 400, "invalid_request")
			return
		}
	}
	id := model.ProfileID(tail[0])
	if action == "search" {
		items, err := backend.SearchDiscoveryFolders(r.Context(), id, strings.TrimSpace(in.Input))
		if err != nil {
			respondError(w, 503, "discovery_unavailable")
			return
		}
		if items == nil {
			items = []manager.DiscoveryCandidate{}
		}
		for i := range items {
			if !safeHTTPS(items[i].URL) {
				items[i].URL = ""
			}
		}
		respond(w, 200, items)
		return
	}
	var v manager.DiscoveryView
	var err error
	switch action {
	case "login":
		v, err = backend.StartDiscoveryLogin(r.Context(), id, in.Origin)
	case "complete":
		v, err = backend.CompleteDiscoveryLogin(r.Context(), id)
	case "rescan":
		v, err = backend.RescanDiscovery(r.Context(), id)
	case "add":
		v, err = backend.AddDiscoveryFolder(r.Context(), id, strings.TrimSpace(in.Input))
	}
	if errors.Is(err, manager.ErrDiscoveryUnclassified) {
		respondError(w, 409, "discovery_unclassified")
		return
	}
	if err != nil {
		respondError(w, 503, "discovery_unavailable")
		return
	}
	respond(w, 200, publicDiscovery(v))
}
