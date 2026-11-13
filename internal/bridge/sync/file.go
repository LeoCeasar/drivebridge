package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"path"

	"github.com/rclone/rclone/internal/bridge/model"
)

func (e *Engine) syncFile(ctx context.Context, j model.Job, en model.Entry) error {
	journal, err := e.store.Journal(ctx, j.ID)
	if err != nil {
		return err
	}
	desired := j.Local
	if journal.Desired != "" {
		desired = journal.Desired
	}
	actual, err := e.observeFile(ctx, en.Token)
	if err != nil {
		checked := e.deletionAfterReadFailure(ctx, en, err)
		if errors.Is(checked, model.ErrVersionChanged) {
			return checked
		}
		return e.pause(ctx, j, checked)
	}
	if err = e.store.ProtectBackup(ctx, j.ID, actual.ID); err != nil {
		return e.pause(ctx, j, err)
	}
	if actual.FileData == desired {
		return e.confirm(ctx, j, actual)
	}
	if (j.State == model.StatusAwaitingVerification && !journal.PreparationRestart) || j.State == model.StatusUploading || journal.Job.Plan != "" {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		return model.ErrUnknownResult
	}
	if model.IsMarkdownFile(en) {
		desired, actual, err = e.planMarkdownFile(ctx, j, en, actual)
		if err != nil {
			return err
		}
		if actual.FileData == desired {
			return e.confirm(ctx, j, actual)
		}
	}
	if !e.opt.EmptyFileUploadVerified && isEmpty(desired) {
		if err = e.store.PauseJob(ctx, j.ID, model.StatusBlocked, "empty_file_unsupported"); err != nil {
			return err
		}
		return model.ErrUnsupportedContent
	}
	if err = e.store.RecordPlan(ctx, j.ID, nil, desired); err != nil {
		return err
	}
	reader, err := e.store.ReadBlob(ctx, desired)
	if err != nil {
		return e.pause(ctx, j, err)
	}
	name := en.CloudName
	if name == "" {
		name = path.Base(en.Remote)
	}
	_, uploadErr := e.source.UploadFile(ctx, "", en.Token, name, reader)
	closeErr := reader.Close()
	if errors.Is(uploadErr, cloud.ErrNotDispatched) {
		if err = e.store.ResetUndispatched(context.WithoutCancel(ctx), j.ID); err != nil {
			return err
		}
		return e.pause(ctx, j, uploadErr)
	}
	if uploadErr != nil || closeErr != nil {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
	}
	readback, err := e.observeFile(ctx, en.Token)
	if err != nil {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		return errors.Join(uploadErr, closeErr, err)
	}
	if err = e.store.ProtectBackup(ctx, j.ID, readback.ID); err != nil {
		return err
	}
	if readback.FileData != desired {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		return errors.Join(model.ErrUnknownResult, uploadErr, closeErr)
	}
	return e.confirm(ctx, j, readback)
}

// This is a local content-addressed blob identity, never a server revision.
func isEmpty(id model.BlobID) bool {
	return string(id) == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}
