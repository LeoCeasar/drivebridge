package syncer

import (
	"context"
	"errors"

	"github.com/rclone/rclone/internal/bridge/doccache"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

// remoteMissing proves absence only from a successful complete parent scan.
// A failed download/403/timeout is never itself a deletion signal.
func (e *Engine) remoteMissing(ctx context.Context, entry model.Entry) ([]larkcli.DriveItem, bool, error) {
	if entry.Token == "" {
		return nil, false, model.ErrIdentityChanged
	}
	parent, err := e.parentToken(ctx, entry.Remote)
	if err != nil {
		return nil, false, err
	}
	items, err := e.source.List(ctx, parent)
	if err != nil {
		return nil, false, err
	}
	seen := make(map[string]bool, len(items))
	missing := true
	for _, item := range items {
		if item.Token == "" || item.Name == "" || item.Type == "" || seen[item.Token] || (item.ParentToken != "" && item.ParentToken != parent) {
			return nil, false, larkcli.ErrProtocol
		}
		seen[item.Token] = true
		if item.Token == entry.Token {
			if item.Type != string(entry.Kind) {
				return nil, false, model.ErrIdentityChanged
			}
			missing = false
		}
	}
	return items, missing, nil
}

func (e *Engine) commitDeletion(ctx context.Context, entry model.Entry, commit func() (bool, error)) error {
	if e.deps.DeletionCommit != nil {
		return e.deps.DeletionCommit(ctx, entry, commit)
	}
	_, err := commit()
	return err
}

func (e *Engine) observeDeletion(ctx context.Context, entry model.Entry) (out model.Conflict, removed bool, err error) {
	err = e.commitDeletion(ctx, entry, func() (bool, error) {
		var commitErr error
		out, removed, commitErr = e.store.ObserveRemoteDeletion(ctx, entry)
		return removed, commitErr
	})
	return
}

func (e *Engine) acceptDeletionDecision(ctx context.Context, entry model.Entry, request model.ResolveRequest) (accepted model.Entry, job model.Job, err error) {
	err = e.commitDeletion(ctx, entry, func() (bool, error) {
		var commitErr error
		accepted, job, commitErr = e.store.ResolveRemoteDeletion(ctx, request)
		return request.Action == model.ResolveAcceptDelete, commitErr
	})
	return
}

func (e *Engine) deletionAfterReadFailure(ctx context.Context, entry model.Entry, readErr error) error {
	if errors.Is(readErr, doccache.ErrIncomplete) {
		return readErr
	}
	_, missing, err := e.remoteMissing(ctx, entry)
	if err != nil {
		return errors.Join(readErr, err)
	}
	if !missing {
		return readErr
	}
	_, _, err = e.observeDeletion(ctx, entry)
	if err != nil {
		return errors.Join(readErr, err)
	}
	return model.ErrVersionChanged
}
