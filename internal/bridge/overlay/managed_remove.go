package overlay

import (
	"context"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

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
