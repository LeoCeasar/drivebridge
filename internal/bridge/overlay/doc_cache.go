package overlay

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/store"

	"github.com/rclone/rclone/internal/bridge/doccache"
	"github.com/rclone/rclone/internal/bridge/model"
)

// ensureDocImages upgrades only reconstructible media cache. The accepted
// snapshot, draft, baseline and journal are immutable, even for a dirty document.
func (f *Fs) ensureDocImages(ctx context.Context, en model.Entry) error {
	if en.Kind != model.KindDocx || en.RemoteCheckpoint == "" {
		return nil
	}
	f.docMediaMu.Lock()
	defer f.docMediaMu.Unlock()
	sn, err := f.store.Snapshot(ctx, en.RemoteCheckpoint)
	if err != nil {
		return err
	}
	return doccache.Ensure(ctx, f.store, f.source, sn)
}

// PrepareManagedRead runs after VFS path normalization and admission, including
// warm VFS-cache hits. Unlike the admission validator this hook may perform I/O.
func (f *Fs) PrepareManagedRead(ctx context.Context, remote string) error {
	en, err := f.store.EntryByRemote(ctx, remote)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.ensureDocImages(ctx, en)
}
