package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"path"

	"github.com/rclone/rclone/internal/bridge/model"
)

func (e *Engine) syncFile(ctx context.Context, j model.Job, en model.Entry) error {
	actual, err := e.observeFile(ctx, en.Token)
	if err != nil {
		return e.pause(ctx, j, err)
	}
	if actual.FileData == j.Local {
		return e.confirm(ctx, j, actual)
	}
	if !e.opt.EmptyFileUploadVerified && isEmpty(j.Local) {
		if err = e.store.PauseJob(ctx, j.ID, model.StatusBlocked, "empty_file_unsupported"); err != nil {
			return err
		}
		return model.ErrUnsupportedContent
	}
	journal, err := e.store.Journal(ctx, j.ID)
	if err != nil {
		return err
	}
	if j.State == model.StatusAwaitingVerification || j.State == model.StatusUploading || journal.Job.Plan != "" {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		return model.ErrUnknownResult
	}
	if err = e.store.ProtectBackup(ctx, j.ID, actual.ID); err != nil {
		return e.pause(ctx, j, err)
	}
	if err = e.store.RecordPlan(ctx, j.ID, nil, j.Local); err != nil {
		return err
	}
	reader, err := e.store.ReadBlob(ctx, j.Local)
	if err != nil {
		return e.pause(ctx, j, err)
	}
	_, uploadErr := e.source.UploadFile(ctx, "", en.Token, path.Base(en.Remote), reader)
	closeErr := reader.Close()
	if errors.Is(uploadErr, cloud.ErrNotDispatched) {
		if err = e.store.ResetUndispatched(ctx, j.ID); err != nil {
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
	if readback.FileData != j.Local {
		_ = e.store.MarkUnknown(context.WithoutCancel(ctx), j.ID)
		return errors.Join(model.ErrUnknownResult, uploadErr, closeErr)
	}
	return e.confirm(ctx, j, readback)
}

// This is a local content-addressed blob identity, never a server revision.
func isEmpty(id model.BlobID) bool {
	return string(id) == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
}
