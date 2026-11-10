package syncer

import (
	"context"
	"errors"
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
