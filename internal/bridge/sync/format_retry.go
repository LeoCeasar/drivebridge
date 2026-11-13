package syncer

import (
	"bytes"
	"context"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"github.com/rclone/rclone/internal/bridge/model"
)

func (e *Engine) retryCompatibilityConflict(ctx context.Context, j model.Job) (model.Job, error) {
	conflicts, err := e.store.Conflicts(ctx)
	if err != nil {
		return j, err
	}
	for _, c := range conflicts {
		if c.EntryID != j.EntryID || c.Generation != j.Generation {
			continue
		}
		var changed bool
		equivalentDraft := false
		var normalizedLocal, normalizedDraft []byte
		if c.Draft != "" && c.Draft != c.Local {
			local, er := e.read(ctx, c.Local)
			if er != nil {
				return j, er
			}
			draft, er := e.read(ctx, c.Draft)
			if er != nil {
				return j, er
			}
			normalizedLocal, er = markdown.NormalizeForMerge(local)
			if er != nil {
				continue
			}
			normalizedDraft, er = markdown.NormalizeForMerge(draft)
			if er != nil {
				continue
			}
			equivalentDraft = bytes.Equal(normalizedLocal, normalizedDraft)
			if !equivalentDraft && markdown.EquivalentForSync(normalizedLocal, normalizedDraft) {
				// Use equal validated codec bodies as the transaction proof;
				// the stored original Markdown and draft remain unchanged.
				normalizedLocal, er = markdown.Encode(normalizedLocal)
				if er != nil {
					continue
				}
				normalizedDraft, er = markdown.Encode(normalizedDraft)
				if er != nil {
					continue
				}
				equivalentDraft = bytes.Equal(normalizedLocal, normalizedDraft)
			}
			if !equivalentDraft {
				continue
			}
		}
		if equivalentDraft && c.Reason == "unsupported_content" {
			changed, err = e.store.RetryEquivalentDraftConflict(ctx, c, normalizedLocal, normalizedDraft)
		} else if c.Reason == "unsupported_content" {
			changed, err = e.store.RetryCompatibilityConflict(ctx, c)
		} else {
			if c.Base == "" || c.Local == "" || c.Remote == "" || (c.Draft != "" && c.Draft != c.Local && !equivalentDraft) {
				continue
			}
			base, readErr := e.read(ctx, c.Base)
			if readErr != nil {
				return j, readErr
			}
			local, readErr := e.read(ctx, c.Local)
			if readErr != nil {
				return j, readErr
			}
			remote, readErr := e.read(ctx, c.Remote)
			if readErr != nil {
				return j, readErr
			}
			for i, b := range [][]byte{base, local, remote} {
				n, er := markdown.NormalizeForMerge(b)
				if er != nil {
					return j, er
				}
				switch i {
				case 0:
					base = n
				case 1:
					local = n
				case 2:
					remote = n
				}
			}
			_, clean, mergeErr := history.MergeText(ctx, base, local, remote)
			if mergeErr != nil {
				return j, mergeErr
			}
			if !clean {
				continue
			}
			if equivalentDraft {
				changed, err = e.store.RetryEquivalentDraftConflict(ctx, c, normalizedLocal, normalizedDraft)
			} else {
				changed, err = e.store.RetryCleanTextConflict(ctx, c)
			}
		}
		if err != nil {
			return j, err
		}
		if changed {
			return e.store.Job(ctx, j.ID)
		}
	}
	return j, nil
}
