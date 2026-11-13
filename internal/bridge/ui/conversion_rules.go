package ui

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rclone/rclone/internal/bridge/manager"
)

// ConversionRulesBackend is optional so existing UI hosts can retain their own backend.
type ConversionRulesBackend interface {
	ConversionRules(context.Context) (manager.ConversionRulesView, error)
	SaveConversionRules(context.Context, string, json.RawMessage) (manager.ConversionRulesView, error)
}

func (b *ManagerBackend) ConversionRules(ctx context.Context) (manager.ConversionRulesView, error) {
	return b.manager.ConversionRules(ctx)
}
func (b *ManagerBackend) SaveConversionRules(ctx context.Context, revision string, document json.RawMessage) (manager.ConversionRulesView, error) {
	return b.manager.SaveConversionRules(ctx, revision, document)
}

func conversionRulesError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, manager.ErrConversionRulesInvalid):
		respond(w, http.StatusBadRequest, map[string]any{"code": "conversion_rules_invalid", "message": err.Error(), "retryable": false})
	case errors.Is(err, manager.ErrConversionRulesChanged):
		respond(w, http.StatusConflict, map[string]any{"code": "conversion_rules_changed", "message": "规则文件已被其他操作修改，本次未覆盖；草稿已保留。请先保留当前编辑，再重新打开页面读取最新规则。", "retryable": false})
	default:
		apiError(w, err)
	}
}

func (h *Host) conversionRulesRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		method(w, "GET, PUT")
		return
	}
	backend, ok := h.backend.(ConversionRulesBackend)
	if !ok {
		respondError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	var view manager.ConversionRulesView
	var err error
	if r.Method == http.MethodGet {
		view, err = backend.ConversionRules(r.Context())
	} else {
		var in struct {
			Revision string          `json:"revision"`
			Document json.RawMessage `json:"document"`
		}
		if decode(w, r, &in) != nil {
			respondError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		digest, digestErr := hex.DecodeString(in.Revision)
		doc := bytes.TrimSpace(in.Document)
		if digestErr != nil || len(digest) != 32 || len(doc) == 0 || doc[0] != '{' {
			respondError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		view, err = backend.SaveConversionRules(r.Context(), in.Revision, in.Document)
	}
	if err != nil {
		conversionRulesError(w, err)
		return
	}
	respond(w, http.StatusOK, view)
}
