package syncer

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"path"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func (e *Engine) syncNamespace(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.namespaceGate:
	}
	defer func() { e.namespaceGate <- struct{}{} }()
	list, err := e.store.NamespaceSchedule(ctx)
	if err != nil {
		return err
	}
	start := 0
	for i, w := range list {
		if w.Ordinal > e.namespaceCursor {
			start = i
			break
		}
	}
	var failures []error
	completed := map[string]bool{}
	for n := 0; n < len(list); n++ {
		i := (start + n) % len(list)
		work := list[i]
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		if !work.Ready {
			continue
		}
		blocked := false
		for j := 0; j < i; j++ {
			if !completed[list[j].Op.ID] && relatedNamespaceWork(list[j], work) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		e.namespaceCursor = work.Ordinal
		// Whole-file recovery backups retain the caller's transfer budget. Only
		// namespace list reads use short slices, and the cursor survives timeouts.
		err = e.namespaceAction(ctx, work.Op)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		current, readErr := e.store.NamespaceJournal(ctx, work.Op.ID)
		if readErr != nil {
			failures = append(failures, readErr)
			continue
		}
		completed[work.Op.ID] = !current.ActionPending
	}
	return errors.Join(failures...)
}
func relatedNamespaceWork(a, b store.NamespaceWork) bool {
	if relatedNamespace(a.Op, b.Op) {
		return true
	}
	for _, left := range a.Bindings {
		if left.Token != "" {
			for _, right := range b.Bindings {
				if left.Token == right.Token {
					return true
				}
			}
		}
	}
	return false
}
func (e *Engine) namespaceList(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	readCtx, cancel := context.WithTimeout(ctx, e.opt.SliceDuration)
	defer cancel()
	return e.source.List(readCtx, parent)
}

func relatedNamespace(a, b model.NamespaceOp) bool {
	for _, left := range []model.EntryID{a.SrcID, a.DstID, a.ResultID} {
		if left == "" {
			continue
		}
		for _, right := range []model.EntryID{b.SrcID, b.DstID, b.ResultID} {
			if left == right {
				return true
			}
		}
	}
	for _, left := range []string{a.Src, a.Dst} {
		if left == "" {
			continue
		}
		for _, right := range []string{b.Src, b.Dst} {
			if right != "" && (left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")) {
				return true
			}
		}
	}
	return false
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
	items, err := e.namespaceList(ctx, parent)
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
		items, err = e.namespaceList(ctx, parent)
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
		items, err = e.namespaceList(ctx, parent)
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
		if work.Op.Kind == "delete" && step == "delete" && work.Step == step {
			confirmed, err := e.reconcileNamespaceDelete(ctx, work, en)
			if err != nil {
				return err
			}
			if confirmed {
				return nil
			}
		}
		return model.ErrUnknownResult
	}
	if !work.Completed[step] {
		if step == "delete" && work.Op.Kind == "delete" && en.Kind != model.KindFolder {
			missing, err := e.reconcileNamespaceDelete(ctx, work, en)
			if err != nil {
				return err
			}
			if missing {
				return nil
			}
		}
		if en.Kind == model.KindFolder {
			children, err := e.namespaceList(ctx, en.Token)
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
		// removal. Lost responses are reconciled separately against the immutable
		// original parent and fulfill only removal from that synchronized directory.
		if step == "delete" && work.Op.Kind == "delete" {
			confirmed, err := e.reconcileNamespaceDelete(ctx, work, en)
			if err != nil {
				return err
			}
			if !confirmed {
				return model.ErrUnknownResult
			}
			return nil
		}
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
		items, err := e.namespaceList(ctx, parent)
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

// Absence is scoped to the captured original parent; a moved resource is never
// followed to another folder or deleted again. Failed/incomplete reads prove nothing.
func (e *Engine) reconcileNamespaceDelete(ctx context.Context, work store.NamespaceWork, en model.Entry) (bool, error) {
	proof, err := e.store.NamespaceDeleteProof(ctx, work.Op.ID)
	if err != nil {
		return false, err
	}
	if proof.Entry != en {
		return false, model.ErrIdentityChanged
	}
	items, err := e.namespaceList(ctx, proof.ParentToken)
	if err != nil {
		return false, err
	}
	missing, err := missingFromParent(items, proof.ParentToken, en)
	if err != nil || !missing {
		return false, err
	}
	if err = e.store.ConfirmNamespaceDeleteAbsence(ctx, proof); err != nil {
		return false, err
	}
	return true, nil
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
			items, err := e.namespaceList(ctx, parent)
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
	items, err := e.namespaceList(ctx, parent)
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
		items, err = e.namespaceList(ctx, parent)
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
