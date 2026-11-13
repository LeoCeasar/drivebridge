package syncer

import (
	"bytes"
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/history"
	"io"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// readMergeBody bounds memory for ordinary files, which can be larger than the
// editable Markdown limit. Their durable originals are never truncated.
func (e *Engine) readMergeBody(ctx context.Context, id model.BlobID) ([]byte, bool, error) {
	r, err := e.store.ReadBlob(ctx, id)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, store.LargeSnapshotBytes+1))
	return b, int64(len(b)) <= store.LargeSnapshotBytes, err
}

// planMarkdownFile preserves local and remote baselines independently: a newer
// local save may still be based on pre-publication bytes while the checkpoint's
// cloud body already includes an earlier merge. Merge the original local base,
// local intent and fresh cloud body; never replace that base with cloud bytes.
func (e *Engine) planMarkdownFile(ctx context.Context, j model.Job, en model.Entry, actual model.Snapshot) (model.BlobID, model.Snapshot, error) {
	fail := func(reason string) (model.BlobID, model.Snapshot, error) {
		return "", actual, e.conflict(ctx, j, actual, reason)
	}
	if j.BaseLocal == "" || j.BaseRemote == "" {
		return fail("missing_base")
	}
	prior, err := e.store.Snapshot(ctx, j.BaseRemote)
	if err != nil {
		return "", actual, e.pause(ctx, j, err)
	}
	if prior.Token != en.Token || prior.FileData == "" {
		return "", actual, e.pause(ctx, j, model.ErrIdentityChanged)
	}
	base, ok, err := e.readMergeBody(ctx, j.BaseLocal)
	if err != nil {
		return "", actual, e.pause(ctx, j, err)
	}
	if !ok {
		return fail("merge_complexity")
	}
	local, ok, err := e.readMergeBody(ctx, j.Local)
	if err != nil {
		return "", actual, e.pause(ctx, j, err)
	}
	if !ok {
		return fail("merge_complexity")
	}
	for attempt := 0; attempt < 3; attempt++ {
		remote, ok, err := e.readMergeBody(ctx, actual.FileData)
		if err != nil {
			return "", actual, e.pause(ctx, j, err)
		}
		if !ok {
			return fail("merge_complexity")
		}
		merged, reason, err := e.mergeText(ctx, base, local, remote)
		if errors.Is(err, history.ErrIntegrity) {
			return fail("unsupported_content")
		}
		if err != nil {
			return "", actual, e.pause(ctx, j, err)
		}
		if reason != "" {
			return fail(reason)
		}
		// This fresh read narrows the last check/write window; the remote API has no
		// atomic compare-and-swap. Back up every observed version before dispatch.
		checked, err := e.observeFile(ctx, en.Token)
		if err != nil {
			return "", actual, e.pause(ctx, j, err)
		}
		if err = e.store.ProtectBackup(ctx, j.ID, checked.ID); err != nil {
			return "", actual, e.pause(ctx, j, err)
		}
		if checked.FileData != actual.FileData {
			actual = checked
			continue
		}
		desired, err := e.store.PutBlob(ctx, bytes.NewReader(merged))
		if err != nil {
			return "", actual, e.pause(ctx, j, err)
		}
		return desired, checked, nil
	}
	return "", actual, e.pause(ctx, j, errRemoteDrift)
}
