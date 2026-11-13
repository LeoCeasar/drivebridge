package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"path"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func (e *Engine) syncNamespace(ctx context.Context) error {
	list, err := e.store.NamespaceActions(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, op := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = e.namespaceAction(ctx, op); err != nil {
			failures = append(failures, err)
			break
		}
	}
	return errors.Join(failures...)
}
func listedToken(items []larkcli.DriveItem, token string) (larkcli.DriveItem, bool) {
	for _, item := range items {
		if item.Token == token {
			return item, true
		}
	}
	return larkcli.DriveItem{}, false
}
func (e *Engine) namespaceAction(ctx context.Context, op model.NamespaceOp) error {
	id := op.SrcID
	if op.Kind == "mkdir" || op.Kind == "replace" {
		id = op.ResultID
	}
	unlock, err := e.entryLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	work, err := e.store.NamespaceJournal(ctx, op.ID)
	if err != nil {
		return err
	}
	if work.SafeSaveBackup {
		if work.SafeSaveCompleted {
			return nil
		}
		fs.Debugf(nil, "safe_save_pending: backup rename awaits durable replacement")
		return store.ErrSafeSavePending
	}
	en, err := e.store.Entry(ctx, id)
	if err != nil {
		return err
	}
	if e.deps.ShouldSyncPath != nil {
		for _, remote := range []string{op.Src, op.Dst, en.Remote} {
			if allowed, err := e.deps.ShouldSyncPath(ctx, remote); err != nil || !allowed {
				return err
			}
		}
	}
	if e.deps.LocalOnly != nil && e.deps.LocalOnly(en.Remote) {
		return e.store.AcknowledgeNamespaceAction(ctx, op.ID)
	}
	if op.Kind == "mkdir" {
		return e.createFolder(ctx, work, en)
	}
	if op.Kind == "replace" {
		src, err := e.store.Entry(ctx, op.SrcID)
		if err != nil {
			return err
		}
		if src.Token == "" {
			return e.store.AcknowledgeNamespaceAction(ctx, op.ID)
		}
		if en.Status != model.StatusSynced {
			return store.ErrBusy
		}
		return e.deleteResource(ctx, work, src, "cleanup_source")
	}
	if op.Kind == "delete" {
		if en.Token == "" {
			return e.store.AcknowledgeNamespaceAction(ctx, op.ID)
		}
		return e.deleteResource(ctx, work, en, "delete")
	}
	if en.Token == "" {
		return e.store.AcknowledgeNamespaceAction(ctx, op.ID)
	}
	parent, err := e.parentToken(ctx, op.Dst)
	if err != nil {
		return err
	}
	desired := path.Base(op.Dst)
	if en.Kind == model.KindDocx {
		desired = cloudName(model.Entry{Remote: op.Dst, Kind: en.Kind})
	}
	items, err := e.source.List(ctx, parent)
	if err != nil {
		return err
	}
	item, atDestination := listedToken(items, en.Token)
	if !atDestination {
		if work.Dispatched {
			return model.ErrUnknownResult
		}
		if err = e.store.BeginNamespaceDispatch(ctx, op.ID, "move"); err != nil {
			return err
		}
		moveErr := e.source.Move(ctx, en.Token, en.Kind, parent)
		if errors.Is(moveErr, cloud.ErrNotDispatched) {
			return errors.Join(moveErr, e.store.RejectNamespaceDispatch(context.WithoutCancel(ctx), op.ID, "move"))
		}
		items, err = e.source.List(ctx, parent)
		if err != nil {
			return errors.Join(moveErr, err)
		}
		item, atDestination = listedToken(items, en.Token)
		if !atDestination {
			return errors.Join(model.ErrUnknownResult, moveErr)
		}
		if err = e.store.ConfirmNamespaceStep(ctx, op.ID, "move"); err != nil {
			return err
		}
		work, err = e.store.NamespaceJournal(ctx, op.ID)
		if err != nil {
			return err
		}
	} else if work.Dispatched && work.Step == "move" {
		if err = e.store.ConfirmNamespaceStep(ctx, op.ID, "move"); err != nil {
			return err
		}
		work, err = e.store.NamespaceJournal(ctx, op.ID)
		if err != nil {
			return err
		}
	}
	if item.Name != desired {
		if work.Dispatched {
			return model.ErrUnknownResult
		}
		if err = e.store.BeginNamespaceDispatch(ctx, op.ID, "rename"); err != nil {
			return err
		}
		renameErr := e.source.Rename(ctx, en.Token, en.Kind, desired)
		if errors.Is(renameErr, cloud.ErrNotDispatched) {
			return errors.Join(renameErr, e.store.RejectNamespaceDispatch(context.WithoutCancel(ctx), op.ID, "rename"))
		}
		items, err = e.source.List(ctx, parent)
		if err != nil {
			return errors.Join(renameErr, err)
		}
		item, atDestination = listedToken(items, en.Token)
		if !atDestination || item.Name != desired {
			return errors.Join(model.ErrUnknownResult, renameErr)
		}
		if err = e.store.ConfirmNamespaceStep(ctx, op.ID, "rename"); err != nil {
			return err
		}
	} else if work.Dispatched && work.Step == "rename" {
		if err = e.store.ConfirmNamespaceStep(ctx, op.ID, "rename"); err != nil {
			return err
		}
	}
	if err = e.store.RecordCloudName(ctx, en.ID, en.Token, desired); err != nil {
		return err
	}
	return e.store.AcknowledgeNamespaceAction(ctx, op.ID)
}
func (e *Engine) deleteResource(ctx context.Context, work store.NamespaceWork, en model.Entry, step string) error {
	if work.Dispatched {
		return model.ErrUnknownResult
	}
	if !work.Completed[step] {
		if step == "delete" && work.Op.Kind == "delete" && en.Kind != model.KindFolder {
			_, missing, err := e.remoteMissing(ctx, en)
			if err != nil {
				return err
			}
			if missing {
				return e.store.AcknowledgeMissingDelete(ctx, work.Op.ID)
			}
		}
		if en.Kind == model.KindFolder {
			children, err := e.source.List(ctx, en.Token)
			if err != nil {
				return err
			}
			if len(children) > 0 {
				return store.ErrBusy
			}
		}
		// Preserve newly acquired remote bytes before a destructive request.
		if en.Kind == model.KindFile {
			sn, err := e.observeFile(ctx, en.Token)
			if err != nil {
				return err
			}
			if err = e.store.ProtectNamespaceBackup(ctx, work.Op.ID, sn.ID); err != nil {
				return err
			}
		}
		if en.Kind == model.KindDocx {
			sn, _, err := e.fetchDoc(ctx, en.Token)
			if err != nil {
				return err
			}
			if err = e.store.ProtectNamespaceBackup(ctx, work.Op.ID, sn.ID); err != nil {
				return err
			}
		}
		if err := e.store.BeginNamespaceDispatch(ctx, work.Op.ID, step); err != nil {
			return err
		}
		if err := e.source.Delete(ctx, en.Token, en.Kind); err != nil {
			if errors.Is(err, cloud.ErrNotDispatched) {
				return errors.Join(err, e.store.RejectNamespaceDispatch(context.WithoutCancel(ctx), work.Op.ID, step))
			}
			return errors.Join(model.ErrUnknownResult, err)
		}
		// A successful explicit delete result plus a complete parent scan observes
		// removal. After response loss, absence alone never authorizes confirmation.
		original := en.Remote
		for _, old := range work.Before {
			if old.ID == en.ID {
				original = old.Remote
			}
		}
		parent, err := e.parentToken(ctx, original)
		if err != nil {
			return err
		}
		items, err := e.source.List(ctx, parent)
		if err != nil {
			return err
		}
		if _, found := listedToken(items, en.Token); found {
			return model.ErrUnknownResult
		}
		if err = e.store.ConfirmNamespaceStep(ctx, work.Op.ID, step); err != nil {
			return err
		}
	}
	return e.store.AcknowledgeNamespaceAction(ctx, work.Op.ID)
}
func (e *Engine) createFolder(ctx context.Context, work store.NamespaceWork, en model.Entry) error {
	parent, err := e.parentToken(ctx, en.Remote)
	if err != nil {
		return err
	}
	if en.Token == "" {
		temp := work.TemporaryName
		if temp == "" {
			return model.ErrUnknownResult
		}
		if work.Dispatched {
			if work.Step != "create_folder" {
				return model.ErrUnknownResult
			}
			items, err := e.source.List(ctx, parent)
			if err != nil {
				return err
			}
			matches := 0
			for _, item := range items {
				if item.Type == "folder" && item.Name == temp {
					en.Token = item.Token
					matches++
				}
			}
			if matches != 1 {
				return model.ErrUnknownResult
			}
		} else {
			if err = e.store.BeginNamespaceDispatch(ctx, work.Op.ID, "create_folder"); err != nil {
				return err
			}
			token, createErr := e.source.CreateFolder(ctx, parent, temp)
			if errors.Is(createErr, cloud.ErrNotDispatched) {
				return errors.Join(createErr, e.store.RejectNamespaceDispatch(context.WithoutCancel(ctx), work.Op.ID, "create_folder"))
			}
			if createErr != nil || token == "" {
				return errors.Join(model.ErrUnknownResult, createErr)
			}
			en.Token = token
		}
		if err = e.store.BindNamespaceFolder(ctx, work.Op.ID, en.Token); err != nil {
			return err
		}
		if err = e.store.ConfirmNamespaceStep(ctx, work.Op.ID, "create_folder"); err != nil {
			return err
		}
	}
	work, err = e.store.NamespaceJournal(ctx, work.Op.ID)
	if err != nil {
		return err
	}
	items, err := e.source.List(ctx, parent)
	if err != nil {
		return err
	}
	item, found := listedToken(items, en.Token)
	if !found {
		return model.ErrUnknownResult
	}
	if work.Dispatched && work.Step == "create_folder" {
		if item.Name != work.TemporaryName {
			return model.ErrUnknownResult
		}
		if err = e.store.ConfirmNamespaceStep(ctx, work.Op.ID, "create_folder"); err != nil {
			return err
		}
		work, err = e.store.NamespaceJournal(ctx, work.Op.ID)
		if err != nil {
			return err
		}
	}
	if item.Name != path.Base(en.Remote) {
		if work.Dispatched {
			return model.ErrUnknownResult
		}
		if err = e.store.BeginNamespaceDispatch(ctx, work.Op.ID, "rename"); err != nil {
			return err
		}
		renameErr := e.source.Rename(ctx, en.Token, model.KindFolder, path.Base(en.Remote))
		if errors.Is(renameErr, cloud.ErrNotDispatched) {
			return errors.Join(renameErr, e.store.RejectNamespaceDispatch(context.WithoutCancel(ctx), work.Op.ID, "rename"))
		}
		items, err = e.source.List(ctx, parent)
		if err != nil {
			return errors.Join(renameErr, err)
		}
		item, found = listedToken(items, en.Token)
		if !found || item.Name != path.Base(en.Remote) {
			return errors.Join(model.ErrUnknownResult, renameErr)
		}
		if err = e.store.ConfirmNamespaceStep(ctx, work.Op.ID, "rename"); err != nil {
			return err
		}
	} else if work.Dispatched && work.Step == "rename" {
		if err = e.store.ConfirmNamespaceStep(ctx, work.Op.ID, "rename"); err != nil {
			return err
		}
	}
	if err = e.store.RecordCloudName(ctx, en.ID, en.Token, path.Base(en.Remote)); err != nil {
		return err
	}
	return e.store.AcknowledgeNamespaceAction(ctx, work.Op.ID)
}
