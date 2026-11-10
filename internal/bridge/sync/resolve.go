package syncer

import (
	"bytes"
	"context"
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
