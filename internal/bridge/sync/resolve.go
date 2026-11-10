package syncer

import (
	"bytes"
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Resolve rechecks the captured remote and stages a new local generation. The
// subsequent upload still has the documented check/write race; this is not CAS.
func (e *Engine) Resolve(ctx context.Context, r model.ResolveRequest) (model.Entry, model.Job, error) {

	conflicts, err := e.store.Conflicts(ctx)
	if err != nil {
		return model.Entry{}, model.Job{}, err
	}
	var captured *model.Conflict
	for i := range conflicts {
		if conflicts[i].ID == r.ConflictID {
			captured = &conflicts[i]
			break
		}
	}
	if captured == nil {
		return model.Entry{}, model.Job{}, store.ErrNotFound
	}
	if err = e.store.SaveDraft(ctx, r.ConflictID, bytes.NewReader(r.Content)); err != nil {
		return model.Entry{}, model.Job{}, err
	}
	if err = e.check(ctx); err != nil {
		return model.Entry{}, model.Job{}, err
	}
	unlock, err := e.entryLock(ctx, captured.EntryID)
	if err != nil {
		return model.Entry{}, model.Job{}, err
	}
	defer unlock()
	en, err := e.store.Entry(ctx, captured.EntryID)
	if err != nil {
		return en, model.Job{}, err
	}
	if en.Generation != r.ExpectedGeneration || captured.Generation != r.ExpectedGeneration {
		return en, model.Job{}, model.ErrGenerationChanged
	}
	if en.Token == "" || en.Kind != model.KindDocx {
		return en, model.Job{}, model.ErrUnsupportedContent
	}
	actual, _, err := e.fetchDoc(ctx, en.Token)
	if err != nil {
		return en, model.Job{}, err
	}
	if actual.Revision != r.ExpectedRemoteRevision || actual.Markdown != captured.Remote {
		return en, model.Job{}, model.ErrVersionChanged
	}
	return e.store.ResolveAgainst(ctx, r, actual)
}

// RefreshConflict regenerates versioned inputs while preserving the user's
// draft. On a cloud error it still returns the saved local/captured remote view;
// callers must label that remote stale and Resolve will recheck it before upload.
func (e *Engine) RefreshConflict(ctx context.Context, id model.ConflictID) (model.Conflict, error) {
	list, err := e.store.Conflicts(ctx)
	if err != nil {
		return model.Conflict{}, err
	}
	var c model.Conflict
	for _, item := range list {
		if item.ID == id {
			c = item
			break
		}
	}
	if c.ID == "" {
		return c, store.ErrNotFound
	}
	unlock, err := e.entryLock(ctx, c.EntryID)
	if err != nil {
		return c, err
	}
	defer unlock()
	pending, err := e.store.Pending(ctx)
	if err != nil {
		return c, err
	}
	for _, j := range pending {
		if j.EntryID == c.EntryID && (j.State == model.StatusMerging || j.State == model.StatusUploading || j.State == model.StatusAwaitingVerification) {
			return c, model.ErrUnknownResult
		}
	}
	var cloudErr error
	en, err := e.store.Entry(ctx, c.EntryID)
	if err != nil {
		return c, err
	}
	if en.Kind == model.KindDocx && en.Token != "" {
		cloudErr = e.check(ctx)
		if cloudErr == nil {
			var sn model.Snapshot
			sn, _, cloudErr = e.fetchDoc(ctx, en.Token)
			if cloudErr == nil {
				c.Remote = sn.Markdown
				c.RemoteRevision = sn.Revision
			}
		}
	}
	for range 3 {
		en, err = e.store.Entry(ctx, c.EntryID)
		if err != nil {
			return c, err
		}
		c.Generation = en.Generation
		c.Local = en.Local
		if err = e.store.SaveConflict(ctx, c); errors.Is(err, model.ErrGenerationChanged) {
			continue
		}
		return c, errors.Join(cloudErr, err)
	}
	return c, model.ErrGenerationChanged
}
