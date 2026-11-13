package overlay

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// CommittedError means durable local mutation happened; cache/node reconciliation
// must finish or replay. Callers must not Abort or blindly repeat that mutation.
type CommittedError struct{ Err error }

func (e *CommittedError) Error() string   { return "local namespace committed: " + e.Err.Error() }
func (e *CommittedError) Unwrap() error   { return e.Err }
func (e *CommittedError) Committed() bool { return true }
func safeSaveSibling(src, dst string) bool {
	return model.EditorTildeBackup(src, dst) || parentPath(src) == parentPath(dst) && strings.HasPrefix(path.Base(dst), path.Base(src)+".sb-") && safeSaveDirectory.MatchString(path.Base(dst))
}

func (f *Fs) BeginManagedRename(ctx context.Context, src, dst string) (string, error) {
	if isDocAssetPath(src) || isDocAssetPath(dst) {
		return "", fs.ErrorPermissionDenied
	}
	if err := validPath(src, false); err != nil {
		return "", err
	}
	if err := validPath(dst, false); err != nil {
		return "", err
	}
	if src == dst {
		return "", errors.New("rename source equals destination")
	}
	// A complete listing may introduce the destination mapping. Serialize its
	// classification with namespace preparation so a new virtual target cannot
	// slip between the readonly check and an atomic replacement reservation.
	editorBackup := model.EditorTildeBackup(src, dst)
	f.listMu.Lock()
	defer f.listMu.Unlock()
	if target, err := f.store.EntryByRemote(ctx, dst); err == nil {
		if err = f.requireWritable(ctx, target); err != nil {
			return "", err
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if err := f.requireParent(ctx, parentPath(dst)); err != nil {
		return "", err
	}
	source, err := f.store.EntryByRemote(ctx, src)
	if errors.Is(err, store.ErrNotFound) {
		if err = f.requireParent(ctx, parentPath(src)); err != nil {
			return "", err
		}
		source, err = f.store.EnsureEntry(ctx, src, newKind(src))
	}
	if err != nil {
		return "", err
	}
	if err = f.requireWritable(ctx, source); err != nil {
		return "", err
	}
	if _, err = f.directory(ctx, parentPath(dst)); err != nil {
		return "", err
	}
	kind := "rename"
	target, err := f.store.EntryByRemote(ctx, dst)
	if err == nil {
		if err = f.requireWritable(ctx, target); err != nil {
			return "", err
		}
		if target.Kind == model.KindFolder || source.Kind == model.KindFolder {
			return "", model.ErrUnsupportedContent
		}
		kind = "replace"
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	safeSaveBackup := kind == "rename" && source.Kind == model.KindFile && source.Token != "" && safeSaveSibling(src, dst)
	op, err := f.store.PrepareManagedRename(ctx, model.NamespaceOp{Kind: kind, Src: src, Dst: dst}, editorBackup && source.Token != "" && (source.Kind == model.KindFile || source.Kind == model.KindDocx), newKind(dst), f.localOnlyPath, safeSaveSibling)
	if err != nil {
		return "", err
	}
	if safeSaveBackup && !editorBackup {
		if err = f.store.ConfigureSafeSaveBackup(ctx, op.ID); err != nil {
			_ = f.store.AbortNamespace(ctx, op.ID)
			return "", err
		}
	}
	return op.ID, nil
}
func (f *Fs) RenameManaged(ctx context.Context, id string) (fs.Object, error) {
	if err := f.validateManagedNamespaceCommit(ctx, id); err != nil {
		return nil, err
	}
	_, err := f.store.SealNamespace(ctx, id)
	if err != nil {
		return nil, err
	}
	e, err := f.store.CommitNamespace(ctx, id)
	if err != nil {
		return nil, err
	}
	if e.Kind == model.KindFolder {
		return nil, nil
	}
	o, err := f.object(ctx, e)
	if err != nil {
		return nil, &CommittedError{Err: err}
	}
	return o, nil
}
func (f *Fs) FinishManagedRename(ctx context.Context, id string) error {
	if err := f.store.FinishNamespace(ctx, id); err != nil {
		return &CommittedError{Err: err}
	}
	return nil
}
func (f *Fs) AbortManagedRename(ctx context.Context, id string) error {
	err := f.store.AbortNamespace(ctx, id)
	if errors.Is(err, store.ErrInvalidState) {
		return &CommittedError{Err: err}
	}
	return err
}

// ReplayManagedNamespace must run before managed dirty-cache reload. Prepared or
// sealed operations roll back only their freeze; committed paths reconcile first.
func (f *Fs) ReplayManagedNamespace(ctx context.Context, reconcile func(src, dst string) error) error {
	ops, err := f.store.PendingNamespace(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		switch op.State {
		case "prepared", "sealed":
			if err = f.store.AbortNamespace(ctx, op.ID); err != nil {
				return err
			}
		case "committed":
			if reconcile == nil {
				return &CommittedError{Err: ErrRecoveryRequired}
			}
			if err = reconcile(op.Src, op.Dst); err != nil {
				return &CommittedError{Err: err}
			}
			if err = f.FinishManagedRename(ctx, op.ID); err != nil {
				return err
			}
		default:
			return store.ErrInvalidState
		}
	}
	return nil
}

func (f *Fs) ReplayManagedNamespaceCheckpoints(ctx context.Context, reconcile func(string, string, []model.ManagedCheckpoint) error) error {
	return f.ReplayManagedNamespace(ctx, func(src, dst string) error {
		ops, err := f.store.PendingNamespace(ctx)
		if err != nil {
			return err
		}
		for _, op := range ops {
			if op.State == "committed" && op.Src == src && op.Dst == dst {
				refs, err := f.store.ManagedNamespaceCheckpoints(ctx, op.ID)
				if err != nil {
					return err
				}
				return reconcile(src, dst, refs)
			}
		}
		return ErrRecoveryRequired
	})
}

// Move is a safe local fallback. Managed VFS must use Begin before Flush and
// Finish only after its cache/node update, instead of generic delete-then-Move.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	if isDocAssetPath(remote) || (src != nil && isDocAssetPath(src.Remote())) {
		return nil, fs.ErrorPermissionDenied
	}
	o, ok := src.(*Object)
	if !ok || o.f != f {
		return nil, fs.ErrorCantMove
	}
	id, err := f.BeginManagedRename(ctx, o.Remote(), remote)
	if err != nil {
		return nil, err
	}
	result, err := f.RenameManaged(ctx, id)
	if err != nil {
		var committed interface{ Committed() bool }
		if !errors.As(err, &committed) || !committed.Committed() {
			_ = f.AbortManagedRename(ctx, id)
		}
		return result, err
	}
	if err = f.FinishManagedRename(ctx, id); err != nil {
		return result, err
	}
	return result, nil
}
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	if isDocAssetPath(dir) {
		return fs.ErrorPermissionDenied
	}
	if err := validPath(dir, true); err != nil {
		return err
	}
	if dir == "" {
		return nil
	}
	if e, err := f.store.EntryByRemote(ctx, dir); err == nil {
		if err = f.requireWritable(ctx, e); err != nil {
			return err
		}
		if e.Kind == model.KindFolder {
			return nil
		}
		return fs.ErrorIsFile
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := f.requireParent(ctx, parentPath(dir)); err != nil {
		return err
	}
	op, err := f.store.PrepareNamespace(ctx, model.NamespaceOp{Kind: "mkdir", Dst: dir})
	if err != nil {
		return err
	}
	if f.localOnlyPath(dir, model.KindFolder) {
		if err := f.store.ConfigureLocalOnlyMkdir(ctx, op.ID); err != nil {
			_ = f.store.AbortNamespace(ctx, op.ID)
			return err
		}
	}
	return f.finishLocalNamespace(ctx, op.ID)
}
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if dir == "" {
		return fs.ErrorPermissionDenied
	}
	return f.remove(ctx, dir, true)
}
func (f *Fs) remove(ctx context.Context, remote string, folder bool) error {
	if isDocAssetPath(remote) {
		return fs.ErrorPermissionDenied
	}
	if remote == "" {
		return fs.ErrorPermissionDenied
	}
	if err := validPath(remote, false); err != nil {
		return err
	}
	e, err := f.store.EntryByRemote(ctx, remote)
	if err != nil {
		return mapNotFound(err)
	}
	if err = f.requireWritable(ctx, e); err != nil {
		return err
	}
	if folder != (e.Kind == model.KindFolder) {
		if folder {
			return fs.ErrorIsFile
		}
		return fs.ErrorIsDir
	}
	if folder {
		children, err := f.List(ctx, remote)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			return fs.ErrorDirectoryNotEmpty
		}
	}
	if !folder {
		cancelled, err := f.store.CancelUnsentLocalFile(ctx, e.ID, e.Generation)
		if err != nil {
			return err
		}
		if cancelled {
			return nil
		}
	}
	jobs, err := f.store.Pending(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.EntryID == e.ID && !f.LocalOnly(remote) {
			if j.State == model.StatusMerging || j.State == model.StatusUploading || j.State == model.StatusAwaitingVerification {
				return errors.Join(ErrDeleteConflict, store.ErrBusy)
			}
			// Explicit unlink supersedes already-durable, undispatched edits.
			// CommitNamespace retains recovery records and settles old conflicts.
		}
	}

	if !folder && e.Visible == "" {
		if _, err = f.MaterializeManaged(ctx, remote); err != nil {
			return err
		}
	}
	op, err := f.store.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: remote})
	if err != nil {
		return err
	}
	if _, err = f.store.SealNamespace(ctx, op.ID); err != nil {
		_ = f.store.AbortNamespace(ctx, op.ID)
		return err
	}
	current, err := f.store.Entry(ctx, e.ID)
	if err != nil {
		_ = f.store.AbortNamespace(ctx, op.ID)
		return err
	}
	if current.Generation != e.Generation && !f.LocalOnly(remote) {
		_ = f.store.AbortNamespace(ctx, op.ID)
		return f.deleteConflict(ctx, current)
	}
	if folder {
		children, er := f.store.Entries(ctx, remote)
		if er != nil {
			_ = f.store.AbortNamespace(ctx, op.ID)
			return er
		}
		if len(children) > 0 {
			_ = f.store.AbortNamespace(ctx, op.ID)
			return fs.ErrorDirectoryNotEmpty
		}
	}
	if _, err = f.store.CommitNamespace(ctx, op.ID); err != nil {
		_ = f.store.AbortNamespace(ctx, op.ID)
		return err
	}
	return f.FinishManagedRename(ctx, op.ID)
}
func (f *Fs) deleteConflict(ctx context.Context, e model.Entry) error {
	c := model.Conflict{EntryID: e.ID, Generation: e.Generation, Local: e.Local, Base: e.LocalCheckpoint, Reason: "delete_vs_pending_edit"}
	if e.RemoteCheckpoint != "" {
		sn, err := f.store.Snapshot(ctx, e.RemoteCheckpoint)
		if err != nil {
			return err
		}
		c.Remote = sn.FileData
		if c.Remote == "" {
			c.Remote = sn.Markdown
		}
		c.RemoteRevision = sn.Revision
	}
	if err := f.store.SaveConflict(ctx, c); err != nil {
		return err
	}
	return ErrDeleteConflict
}
func (f *Fs) finishLocalNamespace(ctx context.Context, id string) error {
	if _, err := f.store.SealNamespace(ctx, id); err != nil {
		_ = f.store.AbortNamespace(ctx, id)
		return err
	}
	if _, err := f.store.CommitNamespace(ctx, id); err != nil {
		_ = f.store.AbortNamespace(ctx, id)
		return err
	}
	return f.FinishManagedRename(ctx, id)
}

func (f *Fs) validateManagedNamespaceCommit(ctx context.Context, id string) error {
	// Recheck before commit even if the reservation came from a different local
	// caller. A committed result remains committed and is handled by recovery.
	ops, err := f.store.PendingNamespace(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.ID != id || op.State == "committed" {
			continue
		}
		for _, remote := range []string{op.Src, op.Dst} {
			if remote == "" {
				continue
			}
			e, err := f.store.EntryByRemote(ctx, remote)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err = f.requireWritable(ctx, e); err != nil {
				return err
			}
		}
	}
	return nil
}
