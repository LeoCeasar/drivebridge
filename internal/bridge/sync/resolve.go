package syncer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Resolve rechecks the captured remote and stages a new local generation. The
// subsequent upload still has the documented check/write race; this is not CAS.
func (e *Engine) Resolve(ctx context.Context, r model.ResolveRequest) (model.Entry, model.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, e.opt.InteractiveTimeout)
	defer cancel()
	defer e.beginForeground()()
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
	localDeletion := captured.Reason == model.ConflictLocalDeletePending
	deleted := captured.Reason == model.ConflictRemoteDeleted || localDeletion
	if (deleted && r.Action != model.ResolveRestoreLocal && r.Action != model.ResolveAcceptDelete) || (!deleted && r.Action != "") {
		return model.Entry{}, model.Job{}, model.ErrUnsupportedContent
	}
	if r.Action != model.ResolveAcceptDelete && (r.Content != nil || r.Action == "") {
		if err = e.store.SaveDraft(ctx, r.ConflictID, bytes.NewReader(r.Content)); err != nil {
			return model.Entry{}, model.Job{}, err
		}
	}
	if !localDeletion {
		if err = e.check(ctx); err != nil {
			return model.Entry{}, model.Job{}, err
		}
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
	if localDeletion {
		return e.acceptDeletionDecision(ctx, en, r)
	}
	if captured.Reason == model.ConflictRemoteDeleted {
		if r.Action != model.ResolveRestoreLocal && r.Action != model.ResolveAcceptDelete {
			return en, model.Job{}, model.ErrUnsupportedContent
		}
		items, missing, checkErr := e.remoteMissing(ctx, en)
		if checkErr != nil {
			return en, model.Job{}, checkErr
		}
		if !missing {
			return en, model.Job{}, model.ErrVersionChanged
		}
		if r.Action == model.ResolveRestoreLocal {
			name := en.CloudName
			if name == "" {
				name = cloudName(en)
			}
			for _, item := range items {
				if item.Name == name {
					return en, model.Job{}, model.ErrVersionChanged
				}
			}
		}
		return e.acceptDeletionDecision(ctx, en, r)
	}
	if r.Action != "" {
		return en, model.Job{}, model.ErrUnsupportedContent
	}
	if en.Token == "" || (en.Kind != model.KindDocx && !model.IsMarkdownFile(en)) {
		return en, model.Job{}, model.ErrUnsupportedContent
	}
	var actual model.Snapshot
	if model.IsMarkdownFile(en) {
		if !editablePlainConflictText(r.Content) {
			return en, model.Job{}, model.ErrUnsupportedContent
		}
		for _, id := range []model.BlobID{captured.Base, captured.Local, captured.Remote} {
			if err = e.checkPlainConflictBlob(ctx, id); err != nil {
				return en, model.Job{}, err
			}
		}
		actual, err = e.observeFile(ctx, en.Token)
		if err == nil {
			err = e.checkPlainConflictBlob(ctx, actual.FileData)
		}
	} else {
		actual, _, err = e.fetchEntryDoc(ctx, en)
	}
	if err != nil {
		return en, model.Job{}, err
	}
	if actual.ConflictVersion() != r.ExpectedRemoteRevision || actual.ContentBlob() != captured.Remote {
		return en, model.Job{}, model.ErrVersionChanged
	}
	return e.store.ResolveAgainst(ctx, r, actual)
}

// RefreshConflict regenerates versioned inputs while preserving the user's
// draft. On a cloud error it still returns the saved local/captured remote view;
// callers must label that remote stale and Resolve will recheck it before upload.
func (e *Engine) RefreshConflict(ctx context.Context, id model.ConflictID) (model.Conflict, error) {
	ctx, cancel := context.WithTimeout(ctx, e.opt.InteractiveTimeout)
	defer cancel()
	defer e.beginForeground()()
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
	en, err := e.store.Entry(ctx, c.EntryID)
	if err != nil {
		return c, err
	}
	if en.Token == "" && (c.Reason == "delete_vs_pending_edit" || c.Reason == model.ConflictLocalDeletePending) {
		var fresh model.Conflict
		err = e.commitDeletion(ctx, en, func() (bool, error) {
			var commitErr error
			fresh, commitErr = e.store.ObserveUnsentDeletion(ctx, en)
			return false, commitErr
		})
		if err != nil {
			return c, err
		}
		list, err := e.store.Conflicts(ctx)
		for _, current := range list {
			if current.ID == id {
				return current, err
			}
		}
		return fresh, errors.Join(err, store.ErrNotFound)
	}
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
	if en.Token != "" && (en.Kind == model.KindFile || en.Kind == model.KindDocx) {
		if cloudErr = e.check(ctx); cloudErr != nil {
			return c, cloudErr
		}
		_, missing, scanErr := e.remoteMissing(ctx, en)
		if scanErr != nil {
			return c, scanErr
		}
		if missing {
			fresh, _, missingErr := e.observeDeletion(ctx, en)
			if missingErr == nil && fresh.ID != "" && fresh.ID != c.ID {
				list, err := e.store.Conflicts(ctx)
				if err != nil {
					return c, err
				}
				for _, current := range list {
					if current.ID == c.ID {
						return current, nil
					}
				}
				return c, store.ErrNotFound
			}
			if fresh.ID != "" {
				return fresh, missingErr
			}
			return c, missingErr
		}
		if c.Reason == model.ConflictRemoteDeleted {
			var actual model.Snapshot
			if en.Kind == model.KindFile {
				actual, err = e.observeFile(ctx, en.Token)
			} else {
				actual, _, err = e.fetchEntryDoc(ctx, en)
			}
			if err != nil {
				return c, err
			}
			if err = e.store.RefreshReappearedConflict(ctx, c, actual); err != nil {
				return c, err
			}
			list, err := e.store.Conflicts(ctx)
			for _, current := range list {
				if current.ID == c.ID {
					return current, err
				}
			}
			return c, store.ErrNotFound
		}
	}
	if (en.Kind == model.KindDocx || model.IsMarkdownFile(en)) && en.Token != "" {
		cloudErr = e.check(ctx)
		if cloudErr == nil {
			var sn model.Snapshot
			if model.IsMarkdownFile(en) {
				sn, cloudErr = e.observeFile(ctx, en.Token)
			} else {
				sn, _, cloudErr = e.fetchEntryDoc(ctx, en)
			}
			if cloudErr == nil {
				c.Remote = sn.ContentBlob()
				c.RemoteRevision = sn.ConflictVersion()
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
		if err != nil {
			return c, errors.Join(cloudErr, err)
		}
		// SaveConflict preserves draft writes made while the cloud request was in
		// flight. Return that durable view rather than the pre-request copy.
		list, err = e.store.Conflicts(ctx)
		if err != nil {
			return c, errors.Join(cloudErr, err)
		}
		for _, saved := range list {
			if saved.ID == id {
				return saved, cloudErr
			}
		}
		return model.Conflict{}, errors.Join(cloudErr, store.ErrNotFound)
	}
	return c, model.ErrGenerationChanged
}

// Human text resolution has the same bounded UTF-8 contract as the diff page.
// Read limits are applied before allocation; binary files remain exportable.
func editablePlainConflictText(body []byte) bool {
	return int64(len(body)) <= store.LargeSnapshotBytes && utf8.Valid(body) && bytes.IndexByte(body, 0) < 0
}

func (e *Engine) checkPlainConflictBlob(ctx context.Context, id model.BlobID) error {
	if id == "" {
		return nil
	}
	reader, err := e.store.ReadBlob(ctx, id)
	if err != nil {
		return err
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, store.LargeSnapshotBytes+1))
	if err != nil {
		return err
	}
	if !editablePlainConflictText(body) {
		return model.ErrUnsupportedContent
	}
	return nil
}
