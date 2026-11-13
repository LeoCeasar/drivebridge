package overlay

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Native delete permission checks must stay local and must not prepare a
// deletion: Windows can ask before a FILE_DELETE_ON_CLOSE creation exists.
func (f *Fs) ValidateManagedRemove(ctx context.Context, remote string) error {
	if err := validPath(remote, false); err != nil {
		return err
	}
	if isDocAssetPath(remote) {
		return fs.ErrorPermissionDenied
	}
	e, err := f.store.EntryByRemote(ctx, remote)
	if errors.Is(err, store.ErrNotFound) {
		return f.requireParent(ctx, parentPath(remote))
	}
	if err != nil {
		return err
	}
	if e.FrozenBy != "" {
		return store.ErrFrozen
	}
	return f.requireWritable(ctx, e)
}

func (f *Fs) BeginManagedRemove(ctx context.Context, remote string) (string, error) {
	if err := validPath(remote, false); err != nil {
		return "", err
	}
	e, err := f.store.EntryByRemote(ctx, remote)
	if err != nil {
		return "", mapNotFound(err)
	}
	if err = f.requireWritable(ctx, e); err != nil {
		return "", err
	}
	if e.Kind == model.KindFolder {
		return "", fs.ErrorIsDir
	}
	op, err := f.store.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: remote})
	return op.ID, err
}

func (f *Fs) CommitManagedRemove(ctx context.Context, id string) error {
	if _, err := f.store.SealNamespace(ctx, id); err != nil {
		return err
	}
	_, err := f.store.CommitNamespace(ctx, id)
	return err
}
