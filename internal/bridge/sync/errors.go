package syncer

import (
	"context"
	"errors"
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
	saveErr := e.store.PauseJob(context.WithoutCancel(ctx), j.ID, state, code)
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
